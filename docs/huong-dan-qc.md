# Hướng dẫn cho QC: chạy release, đọc bằng chứng, đưa vào Jira (Zephyr Scale)

Máy chạy chỉ cần Docker. Mọi lệnh chạy từ thư mục gốc của repo.

## 1. Lần đầu: tạo test case trong Zephyr Scale

    ./tk qc cases                       # → out/qc/testcases.csv

Zephyr Scale → **Tests → Import → CSV**, chọn `out/qc/testcases.csv`, ánh xạ cột (Name, Objective,
Precondition, Priority, Status, Labels, Folder, Owner, Coverage (Issues), Step, Test Data, Expected Result).
Mỗi bước là một dòng; dòng đầu của mỗi case chứa các trường của case. Sau khi import, ghi key Zephyr
(vd. `ORD-T12`) vào trường `qc_key:` của tệp YAML tương ứng để kết quả gắn đúng test case.

## 2. Chạy bộ release

    ./tk up --services order-worker
    ./tk run testkit/suites/release.yaml

Kết quả: thư mục `out/<run_id>/` và gói `out/<run_id>.zip` (mở `report.html` trong gói, không cần mạng).
Mã thoát 0 = **GO**, khác 0 = **NO-GO**. Quyết định do các luật cứng trong mục "Cổng release" của báo cáo.

## 3. Đọc báo cáo

- **Cổng release**: từng luật, yêu cầu và thực tế (fail/error, flaky, phản chứng, khớp trigger,
  regression hiệu năng, độ phủ yêu cầu, testcase bị bỏ qua do máy thiếu quyền).
- Mỗi testcase: mục đích, đầu vào, các bước + timeline, assertion (kỳ vọng / thực tế / thời điểm đo),
  đầu ra (snapshot DB, journal mock, mail, log, ảnh chụp UI), biểu đồ Grafana, kết luận có trích bằng chứng,
  và **bằng chứng phản chứng** (case đỏ khi lỗi được cài vào).
- **Nguồn gốc mock**: mock "tự fake theo tài liệu" nghĩa là bên thứ 3 không có sandbox; hợp đồng lấy từ
  tài liệu/OpenAPI của họ, ngày kiểm chứng ghi rõ — đây là rủi ro còn lại cần biết khi ký.
- Toàn vẹn: `./tk verify out/<run_id>` (sha256 từng tệp trong `manifest.json`).

## 4. Đưa kết quả vào Jira

Trong gói có `qc/zephyr-scale/` (executions.zip, test-cycle.json, testcases.csv, HUONG-DAN-IMPORT.md).

    ZEPHYR_TOKEN=<token API Zephyr Scale> ./tk qc push out/<run_id>

tạo một **test cycle** mới với kết quả từng test case (Passed/Failed) và mô tả trỏ tới gói bằng chứng.
Đính kèm `out/<run_id>.zip` vào test cycle hoặc ticket release để người ký xem bằng chứng.
Token chỉ đọc từ biến môi trường, không bao giờ ghi vào tệp hay bằng chứng.

## 5. Testcase mới

Testcase mới (viết tay hoặc do công cụ soạn) ở trạng thái `draft` không chạy trong release. Để duyệt:

    ./tk admit --approve --by <tên QC> testkit/scenarios/.../TC-XXX.yaml

Lệnh chỉ cho duyệt khi case xanh ổn định và đỏ với mọi lỗi được khai báo; người duyệt là người ký.
