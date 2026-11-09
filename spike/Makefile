.PHONY: cli lib harness
cli:
	mkdir -p bin
	go build -o bin/spikecli ./cmd/spikecli
lib:
	mkdir -p bin
	CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -buildmode=c-archive -o bin/libspike.a ./capi
harness: lib
	swift build --package-path swift-harness
