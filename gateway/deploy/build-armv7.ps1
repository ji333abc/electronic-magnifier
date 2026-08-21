$ErrorActionPreference = 'Stop'
$env:CGO_ENABLED = '0'
$env:GOOS = 'linux'
$env:GOARCH = 'arm'
$env:GOARM = '7'
go build -trimpath -ldflags '-s -w' -o lens-gateway-armv7 ./cmd/lens-gateway
Write-Host 'Created gateway/lens-gateway-armv7'
