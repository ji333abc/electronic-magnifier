package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"
)

const (
	autoFocusOwnerID      = "autofocus"
	autoFocusCoarseStep   = 8
	autoFocusFineStep     = 1
	autoFocusFineRadius   = 5
	autoFocusApproachStep = 6
	autoFocusCorrection   = 3
	autoFocusMaxOffset    = 64
	autoFocusFrameSamples = 3
	autoFocusSettleTime   = 500 * time.Millisecond
	autoFocusFrameGap     = 80 * time.Millisecond
	autoFocusTimeout      = 40 * time.Second
	maxSnapshotBytes      = 5 << 20
)

var (
	errAutoFocusUnavailable = errors.New("autofocus is unavailable")
	errAutoFocusBusy        = errors.New("focus motor is busy")
	errIPCSnapshot          = errors.New("IPC snapshot is unavailable")
	errFocusMotor           = errors.New("focus motor control is unavailable")
)

type autoFocusStatus struct {
	Available  bool       `json:"available"`
	Active     bool       `json:"active"`
	State      string     `json:"state"`
	Message    string     `json:"message"`
	Motor      int        `json:"motor,omitempty"`
	Offset     int        `json:"offset"`
	Score      float64    `json:"score,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

type autoFocusController struct {
	mu       sync.Mutex
	status   autoFocusStatus
	cancel   context.CancelFunc
	hub      *controlHub
	esp      *espClient
	motor    MotorConfig
	snapshot func(context.Context) (image.Image, error)
}

func newAutoFocusController(config Config, secrets Secrets, hub *controlHub, esp *espClient) *autoFocusController {
	controller := &autoFocusController{hub: hub, esp: esp}
	for _, motor := range config.Motors {
		if motor.Role == "focus" {
			controller.motor = motor
			break
		}
	}
	controller.status = autoFocusStatus{
		Available: controller.motor.ID != 0,
		State:     "idle",
		Message:   "先手动粗调，再启动小范围自动精调",
		Motor:     controller.motor.ID,
	}
	client := &http.Client{Timeout: 4 * time.Second}
	target := ipcSnapshotTarget(config)
	controller.snapshot = func(ctx context.Context) (image.Image, error) {
		return fetchIPCSnapshot(ctx, client, target, secrets.IPCUser, secrets.IPCPassword)
	}
	return controller
}

func (a *autoFocusController) Status() autoFocusStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

func (a *autoFocusController) Start(user string) (autoFocusStatus, error) {
	a.mu.Lock()
	if !a.status.Available {
		status := a.status
		a.mu.Unlock()
		return status, errAutoFocusUnavailable
	}
	if a.status.Active {
		status := a.status
		a.mu.Unlock()
		return status, errAutoFocusBusy
	}
	ctx, cancel := context.WithTimeout(context.Background(), autoFocusTimeout)
	if !a.hub.acquireSystem(a.motor.ID, autoFocusOwnerID, user+" · 自动精调@"+autoFocusOwnerID, autoFocusTimeout, cancel) {
		cancel()
		status := a.status
		a.mu.Unlock()
		return status, errAutoFocusBusy
	}
	now := time.Now()
	a.cancel = cancel
	a.status = autoFocusStatus{
		Available: true, Active: true, State: "running", Message: "正在分析初始画面…",
		Motor: a.motor.ID, StartedAt: &now,
	}
	status := a.status
	a.mu.Unlock()
	a.hub.broadcastLeases()
	go func() {
		defer cancel()
		a.run(ctx)
	}()
	return status, nil
}

func (a *autoFocusController) Cancel() autoFocusStatus {
	a.mu.Lock()
	if a.cancel != nil {
		a.status.Message = "正在取消自动精调…"
		a.cancel()
	}
	status := a.status
	a.mu.Unlock()
	return status
}

func (a *autoFocusController) Close() {
	a.Cancel()
}

func (a *autoFocusController) run(ctx context.Context) {
	current := 0
	finalState := "failed"
	finalMessage := "自动精调失败"
	finalScore := 0.0
	var finalError error
	fail := func(err error) {
		finalError = err
		finalMessage = focusErrorMessage(err)
	}
	defer func() {
		if a.hub.systemOwns(a.motor.ID, autoFocusOwnerID) {
			stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = a.esp.command(stopCtx, MotorCommand{Motor: a.motor.ID, Action: "release", CommandID: "autofocus-release"})
			cancel()
		}
		a.hub.releaseSystem(a.motor.ID, autoFocusOwnerID)
		if ctx.Err() != nil && finalState != "succeeded" {
			finalState = "canceled"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				finalMessage = "自动精调超时，已停止电机"
			} else {
				finalMessage = "自动精调已取消"
			}
		}
		if finalError != nil && !errors.Is(finalError, context.Canceled) {
			log.Printf("autofocus stopped: %v", finalError)
		}
		a.finish(finalState, finalMessage, current, finalScore)
	}()

	samples := make(map[int]float64)
	initial, err := a.captureScore(ctx)
	if err != nil {
		fail(err)
		return
	}
	samples[0] = initial
	finalScore = initial
	a.progress("正在判断清晰方向…", current, initial)

	if err := a.moveTo(ctx, &current, autoFocusCoarseStep); err != nil {
		fail(err)
		return
	}
	positive, err := a.captureScore(ctx)
	if err != nil {
		fail(err)
		return
	}
	samples[current] = positive

	if err := a.moveTo(ctx, &current, -autoFocusCoarseStep); err != nil {
		fail(err)
		return
	}
	negative, err := a.captureScore(ctx)
	if err != nil {
		fail(err)
		return
	}
	samples[current] = negative

	bestOffset, bestScore := bestFocusSample(samples)
	direction := 0
	if bestOffset > 0 {
		direction = 1
	} else if bestOffset < 0 {
		direction = -1
	} else if positive > negative {
		direction = 1
	} else {
		direction = -1
	}

	if bestOffset != 0 {
		if err := a.moveTo(ctx, &current, bestOffset); err != nil {
			fail(err)
			return
		}
		declines := 0
		for next := bestOffset + direction*autoFocusCoarseStep; absInt(next) <= autoFocusMaxOffset; next += direction * autoFocusCoarseStep {
			a.progress("正在进行粗略搜索…", current, bestScore)
			if err := a.moveTo(ctx, &current, next); err != nil {
				fail(err)
				return
			}
			score, captureErr := a.captureScore(ctx)
			if captureErr != nil {
				fail(captureErr)
				return
			}
			samples[current] = score
			if score > bestScore {
				bestOffset, bestScore = current, score
				declines = 0
			} else {
				declines++
				if declines >= 2 {
					break
				}
			}
		}
	}

	if absInt(bestOffset) == autoFocusMaxOffset {
		_ = a.moveTo(ctx, &current, 0)
		finalMessage = "最佳点超出精调范围，请先手动粗调"
		return
	}

	fineMin := maxInt(-autoFocusMaxOffset, bestOffset-autoFocusFineRadius)
	fineMax := minInt(autoFocusMaxOffset, bestOffset+autoFocusFineRadius)
	a.progress("正在进行精细搜索…", current, bestScore)
	if err := a.moveTo(ctx, &current, fineMin); err != nil {
		fail(err)
		return
	}
	for offset := fineMin; offset <= fineMax; offset += autoFocusFineStep {
		if current != offset {
			if err := a.moveTo(ctx, &current, offset); err != nil {
				fail(err)
				return
			}
		}
		score, captureErr := a.captureScore(ctx)
		if captureErr != nil {
			fail(captureErr)
			return
		}
		samples[current] = score
		if score > bestScore {
			bestOffset, bestScore = current, score
		}
	}

	minimum, maximum := focusSampleRange(samples)
	if maximum <= 0 || (maximum-minimum)/maximum < 0.03 {
		_ = a.moveTo(ctx, &current, 0)
		finalMessage = "画面纹理不足或清晰度变化太小，请手动调整"
		return
	}

	approach := maxInt(-autoFocusMaxOffset, bestOffset-autoFocusApproachStep)
	if err := a.moveTo(ctx, &current, approach); err != nil {
		fail(err)
		return
	}
	if err := a.moveTo(ctx, &current, bestOffset); err != nil {
		fail(err)
		return
	}
	landedScore, err := a.captureScore(ctx)
	if err != nil {
		fail(err)
		return
	}
	finalScore = landedScore
	if focusScoreDropped(bestScore, landedScore) {
		a.progress("正在校正最终落点…", current, landedScore)
		correctedOffset, correctedScore := bestOffset, landedScore
		correctionMax := minInt(autoFocusMaxOffset, bestOffset+autoFocusCorrection)
		for offset := bestOffset + 1; offset <= correctionMax; offset++ {
			if err := a.moveTo(ctx, &current, offset); err != nil {
				fail(err)
				return
			}
			score, captureErr := a.captureScore(ctx)
			if captureErr != nil {
				fail(captureErr)
				return
			}
			if score > correctedScore {
				correctedOffset, correctedScore = offset, score
			}
		}
		if current != correctedOffset {
			approach = maxInt(-autoFocusMaxOffset, correctedOffset-autoFocusApproachStep)
			if err := a.moveTo(ctx, &current, approach); err != nil {
				fail(err)
				return
			}
			if err := a.moveTo(ctx, &current, correctedOffset); err != nil {
				fail(err)
				return
			}
			verifiedScore, captureErr := a.captureScore(ctx)
			if captureErr != nil {
				fail(captureErr)
				return
			}
			correctedScore = verifiedScore
		}
		bestOffset, finalScore = correctedOffset, correctedScore
	}
	finalState = "succeeded"
	finalMessage = "自动精调完成"
}

func (a *autoFocusController) moveTo(ctx context.Context, current *int, target int) error {
	if target == *current {
		return nil
	}
	if !a.hub.systemOwns(a.motor.ID, autoFocusOwnerID) {
		return context.Canceled
	}
	delta := target - *current
	direction := "cw"
	if delta < 0 {
		direction = "ccw"
		delta = -delta
	}
	command := MotorCommand{
		Motor: a.motor.ID, Action: "move", Direction: direction, Steps: delta,
		Speed: a.motor.DefaultSpeed, Mode: "half", Hold: false,
		CommandID: fmt.Sprintf("autofocus-%d", time.Now().UnixMilli()),
	}
	if err := a.esp.command(ctx, command); err != nil {
		return errors.Join(errFocusMotor, fmt.Errorf("move focus motor: %w", err))
	}
	moveDeadline := time.NewTimer(time.Duration(float64(delta)/float64(command.Speed)*float64(time.Second)) + 1500*time.Millisecond)
	defer moveDeadline.Stop()
	ticker := time.NewTicker(60 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-moveDeadline.C:
			return errors.Join(errFocusMotor, errors.New("focus motor movement timed out"))
		case <-ticker.C:
			status, err := a.esp.status(ctx)
			if err != nil {
				return errors.Join(errFocusMotor, fmt.Errorf("read focus motor status: %w", err))
			}
			for _, motor := range status.Motors {
				if motor.Motor == a.motor.ID && !motor.Running && motor.RemainingSteps == 0 {
					*current = target
					return sleepContext(ctx, autoFocusSettleTime)
				}
			}
		}
	}
}

func (a *autoFocusController) captureScore(ctx context.Context) (float64, error) {
	if !a.hub.systemOwns(a.motor.ID, autoFocusOwnerID) {
		return 0, context.Canceled
	}
	scores := make([]float64, 0, autoFocusFrameSamples)
	for i := 0; i < autoFocusFrameSamples; i++ {
		frame, err := a.snapshot(ctx)
		if err != nil {
			return 0, errors.Join(errIPCSnapshot, fmt.Errorf("capture IPC snapshot: %w", err))
		}
		scores = append(scores, tenengradScore(frame))
		if i+1 < autoFocusFrameSamples {
			if err := sleepContext(ctx, autoFocusFrameGap); err != nil {
				return 0, err
			}
		}
	}
	sort.Float64s(scores)
	return scores[len(scores)/2], nil
}

func (a *autoFocusController) progress(message string, offset int, score float64) {
	a.mu.Lock()
	if a.status.Active {
		a.status.Message = message
		a.status.Offset = offset
		a.status.Score = score
	}
	a.mu.Unlock()
}

func (a *autoFocusController) finish(state, message string, offset int, score float64) {
	a.mu.Lock()
	now := time.Now()
	a.status.Active = false
	a.status.State = state
	a.status.Message = message
	a.status.Offset = offset
	a.status.Score = score
	a.status.FinishedAt = &now
	a.cancel = nil
	a.mu.Unlock()
}

func fetchIPCSnapshot(ctx context.Context, client *http.Client, target, user, password string) (image.Image, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if user != "" {
		request.SetBasicAuth(user, password)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return nil, fmt.Errorf("IPC returned %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxSnapshotBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSnapshotBytes {
		return nil, errors.New("IPC snapshot is too large")
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode IPC snapshot header: %w", err)
	}
	if config.Width < 32 || config.Height < 32 || config.Width > 4096 || config.Height > 2160 {
		return nil, fmt.Errorf("unsupported IPC snapshot size %dx%d", config.Width, config.Height)
	}
	frame, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode IPC snapshot: %w", err)
	}
	return frame, nil
}

func tenengradScore(frame image.Image) float64 {
	bounds := frame.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width < 8 || height < 8 {
		return 0
	}
	x0 := bounds.Min.X + width/4
	x1 := bounds.Max.X - width/4
	y0 := bounds.Min.Y + height/4
	y1 := bounds.Max.Y - height/4
	stride := 1
	for ((x1-x0)/stride)*((y1-y0)/stride) > 230400 {
		stride++
	}
	sampleWidth := (x1 - x0 + stride - 1) / stride
	sampleHeight := (y1 - y0 + stride - 1) / stride
	if sampleWidth < 3 || sampleHeight < 3 {
		return 0
	}
	gray := make([]uint8, sampleWidth*sampleHeight)
	for sampleY, y := 0, y0; sampleY < sampleHeight; sampleY, y = sampleY+1, y+stride {
		for sampleX, x := 0, x0; sampleX < sampleWidth; sampleX, x = sampleX+1, x+stride {
			gray[sampleY*sampleWidth+sampleX] = uint8(lumaAt(frame, x, y))
		}
	}
	var total uint64
	var count uint64
	for y := 1; y < sampleHeight-1; y++ {
		for x := 1; x < sampleWidth-1; x++ {
			a := int(gray[(y-1)*sampleWidth+x-1])
			b := int(gray[(y-1)*sampleWidth+x])
			c := int(gray[(y-1)*sampleWidth+x+1])
			d := int(gray[y*sampleWidth+x-1])
			f := int(gray[y*sampleWidth+x+1])
			g := int(gray[(y+1)*sampleWidth+x-1])
			h := int(gray[(y+1)*sampleWidth+x])
			i := int(gray[(y+1)*sampleWidth+x+1])
			gx := -a - 2*d - g + c + 2*f + i
			gy := -a - 2*b - c + g + 2*h + i
			total += uint64(gx*gx + gy*gy)
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return float64(total) / float64(count)
}

func lumaAt(frame image.Image, x, y int) int {
	r, g, b, _ := frame.At(x, y).RGBA()
	return (77*int(r>>8) + 150*int(g>>8) + 29*int(b>>8)) >> 8
}

func bestFocusSample(samples map[int]float64) (int, float64) {
	bestOffset := 0
	bestScore := -1.0
	for offset, score := range samples {
		if score > bestScore || (score == bestScore && absInt(offset) < absInt(bestOffset)) {
			bestOffset, bestScore = offset, score
		}
	}
	return bestOffset, bestScore
}

func focusSampleRange(samples map[int]float64) (float64, float64) {
	minimum := 0.0
	maximum := 0.0
	first := true
	for _, score := range samples {
		if first || score < minimum {
			minimum = score
		}
		if first || score > maximum {
			maximum = score
		}
		first = false
	}
	return minimum, maximum
}

func focusScoreDropped(reference, actual float64) bool {
	return reference > 0 && actual < reference*0.98
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func focusErrorMessage(err error) string {
	if errors.Is(err, context.Canceled) {
		return "自动精调已取消"
	}
	if errors.Is(err, errIPCSnapshot) {
		return "摄像头快照不可用，请检查摄像头电源、网线或地址"
	}
	if errors.Is(err, errFocusMotor) {
		return "镜头控制暂时不可用，已停止精调"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "自动精调超时，已停止电机"
	}
	return "画面或镜头控制暂时不可用，已停止精调"
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
