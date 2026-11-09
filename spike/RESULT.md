# Kết quả spike

Đã hoàn thành, cổng Task 0 đạt. Kết quả chạy thật ở cuối file. `libspike.a` bản arm64: 60.142.560 byte.

## Chạy thử sau khi xử lý môi trường

Tại thư mục `spike`, chạy `make cli lib harness`. Để tự xuất cookie: Chrome DevTools → Application → Cookies → messenger.com; tạo `cookies.json` thủ công dưới dạng object `{ "c_user": "...", "xs": "...", "datr": "..." }` hoặc mảng `{ "name": "...", "value": "..." }`. Không chia sẻ file này. Sau đó chạy:

```sh
./bin/spikecli --cookies cookies.json --platform messenger --data ./spike-data --duration 5m
swift-harness/.build/debug/Harness cookies.json
```

Nhờ người khác gửi tin thường và E2EE để kiểm tra hai luồng; dừng harness bằng Ctrl+C. Dùng tài khoản thử nghiệm. Device Signal được lưu trong `spike-data/e2ee.db`.

Chưa chắc chắn: Swift harness chưa build; callback và vòng đời stop chưa kiểm tra; số thread dựa trên bảng inbox ban đầu; các dạng sự kiện, tin đính kèm, đồng bộ và nhận E2EE chưa xác minh bằng tài khoản thật. Chưa chứng minh spike đạt cổng quyết định Task 0.

## Kết quả chạy thật (2026-11-09, tài khoản phụ)

| Bước | Kết quả |
|---|---|
| CLI: tải inbox | OK, `threads=0` (bảng thread ở trang đầu không chứa thread E2EE) |
| CLI: đăng ký device E2EE | OK, lưu `spike-data/e2ee.db` |
| CLI: kết nối DGW + E2EE | OK |
| CLI: nhận tin E2EE 1-1 | OK, nhận cả tin của đối phương lẫn tin mình gửi từ thiết bị khác (`sender=<self>:4@msgr`) |
| Swift harness (c-archive + callback) | OK, dùng lại device đã lưu, kết nối DGW + E2EE, không crash |

Kết luận: cổng Task 0 đạt, tiếp tục plan.

Điểm cần xử lý ở các task sau:
- Danh sách hội thoại E2EE không nằm trong `LSDeleteThenInsertThread` của trang inbox. Task 4 phải lấy từ sync group 95 hoặc từ dữ liệu E2EE.
- Log `warn` "Skipping dependency with unrecognized type" của messagix rất nhiều. Lõi thật nên hạ log level của messagix xuống `error`.
- Chưa test thread không mã hóa vì tài khoản phụ không có thread loại này.
