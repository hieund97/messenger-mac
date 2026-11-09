# Kết quả spike

Chưa hoàn thành: CLI và c-archive arm64 build được; SwiftPM bị chặn khi chạy manifest (`sandbox-exec: sandbox_apply: Operation not permitted`) trong môi trường hiện tại. Chưa xác minh harness, callback, đăng nhập hay nhận tin thực tế. `libspike.a`: 60.142.560 byte (khoảng 57 MiB). Không chạy kết nối thật.

## Chạy thử sau khi xử lý môi trường

Tại thư mục `spike`, chạy `make cli lib harness`. Để tự xuất cookie: Chrome DevTools → Application → Cookies → messenger.com; tạo `cookies.json` thủ công dưới dạng object `{ "c_user": "...", "xs": "...", "datr": "..." }` hoặc mảng `{ "name": "...", "value": "..." }`. Không chia sẻ file này. Sau đó chạy:

```sh
./bin/spikecli --cookies cookies.json --platform messenger --data ./spike-data --duration 5m
swift-harness/.build/debug/Harness cookies.json
```

Nhờ người khác gửi tin thường và E2EE để kiểm tra hai luồng; dừng harness bằng Ctrl+C. Dùng tài khoản thử nghiệm. Device Signal được lưu trong `spike-data/e2ee.db`.

Chưa chắc chắn: Swift harness chưa build; callback và vòng đời stop chưa kiểm tra; số thread dựa trên bảng inbox ban đầu; các dạng sự kiện, tin đính kèm, đồng bộ và nhận E2EE chưa xác minh bằng tài khoản thật. Chưa chứng minh spike đạt cổng quyết định Task 0.
