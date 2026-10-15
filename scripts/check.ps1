$ErrorActionPreference = 'Stop'
$taskRoot = Split-Path -Parent $PSScriptRoot
Push-Location $taskRoot
$oldModuleCache = $env:GOMODCACHE
$oldBuildCache = $env:GOCACHE
try {
    $env:GOMODCACHE = Join-Path $taskRoot '.cache/gomod'
    $env:GOCACHE = Join-Path $taskRoot '.cache/gobuild'
    go test ./... -count=1
    if ($LASTEXITCODE -ne 0) { throw 'Go tests failed' }
    go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'Go vet failed' }
    $testBinaryName = if ($env:OS -eq 'Windows_NT') { 'ws-test.exe' } else { 'ws-test' }
    $testBinary = Join-Path $taskRoot ".cache/$testBinaryName"
    go build -o $testBinary ./cmd/ws-downloader
    if ($LASTEXITCODE -ne 0) { throw 'Build failed' }
    node --test client/client.test.cjs
    if ($LASTEXITCODE -ne 0) { throw 'Client integration tests failed' }
} finally {
    $env:GOMODCACHE = $oldModuleCache
    $env:GOCACHE = $oldBuildCache
    Pop-Location
}
