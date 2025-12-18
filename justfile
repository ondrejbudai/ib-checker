default:
    @just --list

container := "quay.io/hummingbird/go:latest"
podman_run := "podman run --rm -v " + justfile_directory() + ":/src:Z -w /src"
cache_volumes := "-v ib-checker-gomod:/go/pkg/mod -v ib-checker-gocache:/root/.cache/go-build"
lint_cache := "-v ib-checker-lintcache:/root/.cache/golangci-lint"

# Download dependencies
tidy:
    {{podman_run}} {{cache_volumes}} {{container}} go mod tidy

# Run golangci-lint
lint: tidy
    {{podman_run}} {{cache_volumes}} {{lint_cache}} {{container}} go tool golangci-lint run

# Build the binary
build: tidy
    {{podman_run}} {{cache_volumes}} {{container}} go build -o build/ib-checker .

# Run with specified config
run config *args="": build
    {{podman_run}} -e OFFLINE_TOKEN -e SLACK_WEBHOOK -e TELEGRAM_WEBHOOK -e MESSAGE_PREFIX {{container}} ./build/ib-checker {{config}} {{args}}
