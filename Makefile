# Lưu ý: khi thêm dependency messagix/whatsmeow vào core/go.mod, chép nguyên
# version (và dòng replace req/v3) từ spike/go.mod. Không dùng `go get`.
CORE_OUT := core/build
XCF := $(CORE_OUT)/MessengerCore.xcframework
SLICE := $(XCF)/macos-arm64_x86_64

.PHONY: core kit-test app clean
core:
	rm -rf $(CORE_OUT) && mkdir -p $(CORE_OUT)/arm64 $(CORE_OUT)/amd64 $(SLICE)/Headers
	cd core && CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 CC="clang -arch arm64" MACOSX_DEPLOYMENT_TARGET=14.0 \
		go build -buildmode=c-archive -o build/arm64/libMessengerCore.a ./capi
	cd core && CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 CC="clang -arch x86_64" MACOSX_DEPLOYMENT_TARGET=14.0 \
		go build -buildmode=c-archive -o build/amd64/libMessengerCore.a ./capi
	lipo -create $(CORE_OUT)/arm64/libMessengerCore.a $(CORE_OUT)/amd64/libMessengerCore.a -output $(SLICE)/libMessengerCore.a
	cp core/capi/core.h $(SLICE)/Headers/core.h
	printf 'module MessengerCore {\n    header "core.h"\n    export *\n}\n' > $(SLICE)/Headers/module.modulemap
	cp core/xcframework-Info.plist $(XCF)/Info.plist
	scripts/check-header.sh core/build/arm64/libMessengerCore.h core/capi/core.h

# CLT-only không có XCTest: test dùng swift-testing, cần trỏ tới framework của CLT.
# Có Xcode đầy đủ thì bỏ TESTING_FLAGS và đổi test về XCTest.
CLT_FW := /Library/Developer/CommandLineTools/Library/Developer/Frameworks
TESTING_FLAGS := -Xswiftc -F -Xswiftc $(CLT_FW) -Xlinker -F -Xlinker $(CLT_FW) -Xlinker -rpath -Xlinker $(CLT_FW)

kit-test:
	cd MessengerKit && swift test $(TESTING_FLAGS) $(if $(FILTER),--filter '$(FILTER)')

app: core
	scripts/make-app.sh

clean:
	rm -rf core/build build
