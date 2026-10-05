# Hướng dẫn cho QC: chạy release, đọc bằng chứng, đưa vào Jira (Zephyr Scale)

Máy chạy chỉ cần Docker. Mọi lệnh chạy từ thư mục gốc của repo.

## 1. Danh sách testcase (CSV + Markdown)

    ./tk qc cases                       # → out/qc/testcases.csv và out/qc/testcases.md

- `testcases.md`: đọc trực tiếp (mục đích, tiền điều kiện, từng bước với dữ liệu và kỳ vọng, phản chứng).
- `testcases.csv`: mở bằng Excel hoặc import vào công cụ quản lý test (Jira plugin, TestRail, ...). Mỗi bước
  là một dòng, các cột của case lặp lại ở mọi dòng; cột: Case ID, QC Key, Title, Requirement, Risk, Priority,
  Status, Owner, Service, Triggers, Purpose, Preconditions, Step No, Step, Test Data, Expected Result, Source File.
  Khi đã có mã test case trong công cụ QC, ghi vào `qc_key:` của tệp YAML để các lần xuất sau mang theo mã đó.

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

## 4. Bàn giao kết quả

Trong gói có thư mục `qc/`:

| Tệp | Nội dung |
|-----|----------|
| `results.md` | Tóm tắt cho người đọc: cổng release từng luật, kết quả từng testcase theo trigger, phản chứng, link bằng chứng |
| `results.csv` | Một dòng mỗi lần chạy (case × trigger): kết quả case, kết quả lần chạy, phân loại, lý do, mutation đỏ/tổng, thời gian, đường dẫn bằng chứng — import vào công cụ QC hoặc Excel |
| `testcases.csv` / `.md` | Đúng các testcase của run này (như mục 1) |

Đính kèm `out/<run_id>.zip` vào ticket release để người ký xem bằng chứng (`report.html`).

Sau này chuyển sang công cụ khác (vd. Zephyr Scale) chỉ là thêm bộ xuất: `qc.tool: files, zephyr-scale`
trong `testkit.yaml` (bộ xuất Zephyr Scale đã có sẵn, kèm `ZEPHYR_TOKEN=... ./tk qc push out/<run_id>`).

## 5. Testcase mới

Testcase mới (viết tay hoặc do công cụ soạn) ở trạng thái `draft` không chạy trong release. Để duyệt:

    ./tk admit --approve --by <tên QC> testkit/scenarios/.../TC-XXX.yaml

Lệnh chỉ cho duyệt khi case xanh ổn định và đỏ với mọi lỗi được khai báo; người duyệt là người ký.
