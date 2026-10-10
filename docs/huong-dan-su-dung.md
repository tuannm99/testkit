# Hướng dẫn sử dụng TestKit

Tài liệu này đi từ cài đặt tới viết testcase, chạy và đọc kết quả. Tham chiếu chi tiết:

| Tài liệu | Nội dung |
|---|---|
| [tham-chieu-testcase.md](tham-chieu-testcase.md) | Mọi trường của tệp testcase YAML, bước (step), kiểm tra (check), toán tử, biến, mutation, suite |
| [tham-chieu-service.md](tham-chieu-service.md) | Mô tả một service cần test (`testkit/services/*.yaml`) |
| [tham-chieu-cau-hinh.md](tham-chieu-cau-hinh.md) | `testkit.yaml`, các tệp env, biến môi trường, toàn bộ lệnh CLI |
| [huong-dan-qc.md](huong-dan-qc.md) | Dành cho QC: chạy release, đọc báo cáo, bàn giao kết quả |
| [../AGENTS.md](../AGENTS.md) | Quy tắc cho AI agent làm việc trong repo |

## 1. Khái niệm trong 1 phút

- **Service** (`testkit/services/<tên>.yaml`): mô tả service cần test — image, kho dữ liệu nó dùng, bên thứ 3 nó gọi
  (sẽ được mock), cách giao job cho nó (trigger), các lỗi có thể cài vào (failpoint).
- **Testcase** (`testkit/scenarios/**/*.yaml`): chuẩn bị dữ liệu → làm các bước → **kỳ vọng** có toán tử và giá trị cụ thể.
  Mỗi case chạy qua mọi trigger của service (vd. Kafka và DB poll) và phải cho cùng kết quả.
- **Namespace**: mỗi lần chạy một case có không gian riêng (database, topic, index, tiền tố key, đường dẫn mock, container),
  nên các case chạy song song không đụng nhau và không cần dọn dữ liệu bằng tay.
- **Bằng chứng** (`out/<run_id>/`): báo cáo HTML, log, snapshot DB, journal của mock, ảnh Grafana kèm số liệu thô,
  `manifest.json` chứa sha256 từng tệp. Đóng gói thành `out/<run_id>.zip`.
- **Pass/fail** do luật cứng quyết định: kỳ vọng, ngưỡng SLO, mutation (cài lỗi vào service — case phải đỏ), cổng release.
  AI (nếu bật) chỉ gợi ý.

## 2. Cài đặt

**Cần:** Docker Engine 24+ có Compose v2; 4 CPU, 8–16 GB RAM (Elasticsearch, ClickHouse, Kafka chạy cùng lúc),
~15 GB đĩa trống (image Playwright khoảng 3,7 GB). Không cần cài Go hay Node: `./tk` chạy CLI trong container.

```sh
git clone https://github.com/tuannm99/testkit && cd testkit
./tk doctor
```

`doctor` kiểm tra: Docker/Compose, quyền mount `docker.sock`, `NET_ADMIN` và module `sch_netem` (cho chaos mạng),
giới hạn số file mở (Elasticsearch), cổng còn trống, RAM, đĩa. Mỗi dòng FAIL có kèm cách sửa. Thiếu quyền chaos
không chặn việc dùng TestKit: các case cần quyền đó sẽ bị **bỏ qua và ghi vào báo cáo**, không tính fail.

Lần đầu, `./tk` tự build image `testkit/runner` (chứa CLI); sau mỗi lần sửa code trong `testkit/` nó tự build lại.

Có Go 1.24 thì có thể build CLI chạy trực tiếp trên máy: `make build` rồi dùng `./bin/testkit` thay cho `./tk`.

**Mạng công ty có proxy chặn TLS:** đặt `TESTKIT_BUILD_CA=/đường/dẫn/ca.pem` để các lần build image tin CA đó;
`HTTPS_PROXY`/`NO_PROXY` được chuyển vào build tự động.

## 3. Khởi động hạ tầng

```sh
./tk up --services order-worker                                              # tối thiểu: kho dữ liệu + mock mà service khai báo
./tk up --services order-worker --profile core,stores,mocks,observability,chaos   # đầy đủ: thêm Grafana/Prometheus/Loki và Toxiproxy
./tk status
./tk down                                                                    # xoá sạch container, volume, network (chạy lại bao nhiêu lần cũng được)
```

| Profile | Thành phần |
|---|---|
| `core` | Mock Hub (mock HTTP/webhook, SMTP, WebSocket/TCP) |
| `stores` | Postgres, Kafka, Elasticsearch, ClickHouse, Mongo, Redis — chỉ những gì service khai báo |
| `mocks` | Mailpit (hộp thư nhận mail) |
| `observability` | Prometheus, Grafana (+ renderer ảnh), Loki, Tempo, OTel Collector, Alloy, exporter |
| `chaos` | Toxiproxy |

Không bật `observability` thì case vẫn chạy và vẫn có log, snapshot, journal; chỉ thiếu ảnh Grafana (báo cáo ghi rõ).
Grafana: `http://127.0.0.1:53000`, user `admin`, mật khẩu là `TK_GRAFANA_ADMIN_PASSWORD` trong
`infra/compose/testkit.env` (giá trị chỉ dùng cho test). Mọi cổng chỉ mở trên `127.0.0.1`.

## 4. Chạy test

```sh
./tk lint                                          # kiểm tra mọi testcase (trường bắt buộc, bước/check tồn tại, ...)
./tk plan testkit/scenarios/order/TC-ORDER-017.yaml   # xem trước sẽ làm gì, không chạm hạ tầng
./tk run testkit/scenarios/order/TC-ORDER-017.yaml    # chạy một case
./tk run testkit/scenarios/order                   # chạy cả thư mục
./tk run --mutations --parallel 4 testkit/scenarios    # kèm phản chứng, 4 case song song
./tk run testkit/suites/release.yaml               # bộ release: GO / NO-GO
```

Tuỳ chọn hay dùng của `run`: `--mutations` (chạy cả các lần cài lỗi), `--retries 1` (phát hiện flaky; flaky không bao giờ
tính là pass), `--parallel N`, `--trigger kafka` (chỉ một trigger), `--keep` (giữ container/namespace để gỡ lỗi),
`--build` (build lại image service), `--run-id` (đặt tên run).

Mã thoát: `0` tất cả đạt (với suite: GO), `1` có fail/error hoặc NO-GO, `2` lỗi cú pháp/lint (chưa chạy gì).

## 5. Đọc kết quả

Mỗi lần chạy tạo `out/<run_id>/` (ví dụ `out/r20261005-024858-p6/`):

| Tệp | Nội dung |
|---|---|
| `report.html` | Báo cáo tiếng Việt, mở offline: tổng hợp, cổng release, từng case (mục đích, đầu vào, bước + timeline, kỳ vọng/thực tế/lúc đo, đầu ra, ảnh Grafana, kết luận có trích bằng chứng, phản chứng) |
| `<case>/<trigger>/case.json` | Toàn bộ kết quả một lần chạy |
| `<case>/<trigger>/assertions/*.json` | Từng kỳ vọng: giá trị thực tế, thời điểm, nguồn |
| `<case>/<trigger>/timeline.json` | Từng bước, có thời gian |
| `<case>/<trigger>/logs/`, `output/`, `grafana/` | Log service, snapshot kho, journal mock, mail, ảnh + số liệu thô |
| `junit.xml`, `traceability.csv`, `run.json` | Cho CI và truy vết yêu cầu |
| `qc/results.{csv,md}`, `qc/testcases.{csv,md}` | Bàn giao QC |
| `manifest.json` | sha256 từng tệp; `./tk verify out/<run_id>` báo tệp nào bị sửa |

**Case đỏ — phân loại sơ bộ theo luật** (cột "Phân loại" trong báo cáo):

| Phân loại | Nghĩa | Thường làm gì |
|---|---|---|
| product | Kỳ vọng sai trong khi môi trường khoẻ | Lỗi thật của service: báo dev kèm báo cáo |
| environment | Hạ tầng/khởi tạo/phụ thuộc của bộ test lỗi | Xem `./tk status`, log hạ tầng, chạy lại |
| test | Bản thân kịch bản sai (template, tham số bước) | Sửa testcase |
| flaky | Đỏ rồi xanh khi chạy lại | Tìm nguyên nhân (thường là chạy chung, thứ tự); chỉ người có thẩm quyền cho vào cách ly |
| capability | Máy thiếu quyền khai báo (netem, NET_ADMIN...) | Chạy trên máy có quyền; không tính fail |

**Không bao giờ** sửa kỳ vọng/ngưỡng cho dễ qua để làm xanh một case đỏ. Tìm nguyên nhân trước.

Gỡ lỗi nhanh: `./tk run --keep <case>` giữ container lại; tên container `tk-<run>-<n>-<service>`
(`docker logs`, `docker exec`). Xoá sau khi xong bằng `./tk down` hoặc `docker rm -f`.

## 6. Viết testcase đầu tiên

1. Đọc `testkit/services/order-worker.yaml` (entity, mock, trigger, failpoint có sẵn) và chạy `./tk steps` (từ vựng).
2. Chép một case gần giống làm mẫu, ví dụ `testkit/scenarios/order/TC-ORDER-017.yaml`, tạo
   `testkit/scenarios/drafts/TC-ORDER-050.yaml`:

```yaml
id: TC-ORDER-050
title: Cổng thanh toán trả 500 một lần rồi thành công — đơn paid, gọi cổng đúng 2 lần
requirement: REQ-231
risk: P1
status: draft
service: order-worker
purpose: >
  Lỗi tạm thời của cổng thanh toán được thử lại; đơn thành paid, không gọi thừa.
preconditions:
  - Đơn o50 pending; cổng thanh toán (mock theo OpenAPI) trả 500 rồi 201
input:
  order: { id: o50, customer_email: "cust-o50+{{ .ns }}@shop.test", amount_cents: 5000, currency: USD, status: pending }
trigger: [kafka, db-poll, rabbitmq, redis]   # chạy qua từng đường giao job service hỗ trợ; kết quả phải giống nhau
given:
  postgres.order: ["{{ .input.order }}"]
  mock.payment: [500, 201]
  job: { id: o50 }
expect:
  - { id: A1, check: postgres.order.o50.status, eq: paid, why: "Đơn paid sau khi retry" }
  - { id: A2, check: mock.payment.calls, eq: 2, why: "1 lần lỗi + 1 lần thành công, không gọi thừa" }
  - { id: A3, check: mock.payment.schema_errors, eq: 0, why: "Request đúng hợp đồng OpenAPI" }
evidence:
  grafana: [payment_calls_total, jobs_total]
within: 30s
mutations:
  - { id: M1, failpoint: no_retry, title: "Tắt retry", expect_red: [A1] }
```

3. Kiểm tra và chạy:

```sh
./tk lint testkit/scenarios/drafts/TC-ORDER-050.yaml
./tk plan testkit/scenarios/drafts/TC-ORDER-050.yaml
./tk run --mutations testkit/scenarios/drafts/TC-ORDER-050.yaml
```

4. Cổng duyệt (mutation gate): case phải xanh ổn định (mặc định 2 lần mỗi trigger) và **đỏ** khi lỗi `no_retry` được cài:

```sh
./tk admit testkit/scenarios/drafts/TC-ORDER-050.yaml                       # chỉ báo cáo
./tk admit --approve --by <tên người duyệt> testkit/scenarios/drafts/TC-ORDER-050.yaml   # người duyệt ký
```

`--approve` đổi `status: approved` và ghi khối `admission:` (run, người duyệt, mutation đã đỏ, sha256 bằng chứng).
Sau đó chuyển tệp sang thư mục nghiệp vụ (vd. `scenarios/order/`) để bộ release thấy nó.

Nguyên tắc khi viết:
- Không `sleep`: dùng `within`, `wait.until`, `assert.during` (đều là chờ theo điều kiện).
- Dữ liệu theo lần chạy: địa chỉ, mã định danh chứa `{{ .ns }}`; không dùng dữ liệu thật, không ghi secret.
- Mỗi kỳ vọng phải có thể sai (không viết kỳ vọng luôn đúng); `why` giải thích bằng tiếng Việt cho QC.
- Ít nhất một mutation. Nếu service chưa có failpoint phù hợp, cần dev thêm failpoint (xem tham chiếu service).

## 7. Các loại case khác

### Hiệu năng (perf)

```yaml
perf:
  kind: load                  # smoke | load | stress | spike | soak (nhãn)
  executor: trigger           # trigger: job tới theo nhịp cố định; k6: HTTP bằng script k6
  warmup: 5s                  # không tính vào số đo
  stages: [{ rate: 20, duration: 20s }]   # 20 job/giây trong 20 giây (open model)
  repeat: 3                   # số lần đo (thống kê cần nhiều lần)
  id_prefix: PF
  thresholds: { p95_ms: 1500, error_rate: 0.01, min_throughput: 15, max_dropped: 0 }   # SLO (luật cứng)
  baseline: { key: order-pipeline-20rps, max_regression: 10% }
```

- Baseline gắn với **môi trường** (CPU, RAM, phiên bản Docker, image). Ghi baseline trên stack đang **rảnh**:
  `./tk baseline record --runs 5 testkit/scenarios/perf/TC-PERF-001.yaml`; xem: `./tk baseline show`.
  Thư mục `testkit/baselines/<fingerprint>/` nên commit vào repo.
- Regression = tệ hơn baseline quá ngưỡng (mặc định 10%) **và** có ý nghĩa thống kê (Mann-Whitney U, α = 0,05).
- Kết quả tốt hơn baseline rất nhiều bị đánh dấu "baseline có thể đã cũ": hãy ghi lại baseline.
- Case perf và case tạo tải tự chạy **một mình** (không song song với case khác) để số đo không bị nhiễu.
- `executor: k6`: thêm `script: webhook.js` (cạnh tệp case) và `target: "{{ .sut.http }}"`; xem `scenarios/perf/TC-PERF-002.yaml`.

### Chaos

```yaml
chaos:
  proxies: [payment]          # các phụ thuộc đi qua Toxiproxy (khai báo trong service: chaos.proxies)
  max_recovery: 30s
steps:
  - step: load.start          # tải nền theo nhịp cố định
    with: { rate: 10, from: 1, to: "{{ .input.orders }}", id_prefix: L }
  - step: wait.until          # trạng thái ổn định trước khi gây lỗi
    with: { check: postgres.order.count(status=paid), gte: 20, within: 30s }
  - step: chaos.latency
    with: { proxy: payment, latency: 1500 }
  - step: chaos.hold          # giữ lỗi; dừng khẩn cấp nếu vượt giới hạn
    with:
      for: 8s
      abort_if:
        - { check: kafka.lag(order-worker), gt: 150, why: "dừng nếu tồn đọng vượt 150" }
  - step: chaos.clear
  - step: chaos.recover       # đo thời gian về lại trạng thái ổn định
    with: { within: 60s, expect: [ { check: kafka.lag(order-worker), lte: 5 } ] }
  - step: load.wait
    with: { timeout: 60s }
```

Kỳ vọng thường dùng: `experiment.recovery_seconds`, `experiment.aborted`, `experiment.load.late`,
`reconcile.<tên>.mismatches`. Lỗi trên hạ tầng dùng chung (`chaos.container` với `target` là postgres, kafka...)
tự chạy một mình. Các bước cần quyền (`chaos.netem` cần `netem`) bị bỏ qua và báo cáo khi máy thiếu quyền.

### UI (Playwright)

Test UI viết bằng TypeScript trong `ui-tests/tests/`, chạy trong image Playwright đã ghim phiên bản:

```yaml
steps:
  - step: ui.run
    name: order-status
    with: { spec: orders/order-status.spec.ts, env: { ORDER_ID: u1 } }
expect:
  - { id: A2, check: ui.failed, eq: 0, why: "Mọi UI test đều đạt" }
  - { id: A3, check: ui.passed, eq: 2, why: "Đúng 2 UI test đã chạy" }
```

Trong test, `baseURL` là service đang test (trong mạng Docker). Ảnh chụp màn hình và trace luôn được giữ làm bằng chứng
và hiện ngay trong báo cáo. Không bật retry trong Playwright: flaky do `--retries` của TestKit phát hiện.

### Bộ case chuẩn (sinh tự động từ mô tả service)

Không cần tính đầu vào/đầu ra cho từng case disruptive. Bạn mô tả **nghiệp vụ của service một lần** (dữ liệu của một job,
điều đúng khi job xong, hiệu ứng phụ phải xảy ra đúng một lần) trong mục `conformance:` của `testkit/services/<tên>.yaml`;
`./tk gen --service <tên>` sinh các case nháp: **job giao trùng, message độc → DLQ, chết giữa chừng rồi giao lại, lỗi
phụ thuộc dưới tải (DB/cổng thanh toán), tới ngược thứ tự và phát lại muộn, tải ổn định so với baseline** — cho mọi
trigger service có (Kafka, RabbitMQ, Redis, bảng DB). Case sinh ra là `draft`; chạy `./tk run --mutations`, `./tk admit`,
rồi người duyệt mới `--approve`. Hướng dẫn đầy đủ: [bo-case-chuan.md](bo-case-chuan.md).

## 8. Bộ release và bàn giao

Tệp suite (`kind: Suite`, ví dụ `testkit/suites/release.yaml`) chọn case, cách chạy và **cổng release**:

```sh
./tk run testkit/suites/release.yaml       # chỉ case approved; mutation + retry; GO/NO-GO; tạo out/<run_id>.zip
./tk verify out/<run_id>
./tk pack out/<run_id>                     # đóng gói lại (kiểm tra manifest + không có secret)
./tk qc cases                              # out/qc/testcases.{csv,md}: danh sách testcase cho công cụ QC
```

Cổng NO-GO khi có: fail/error; flaky không nằm trong danh sách cách ly còn hạn; mutation sống sót; kết quả khác nhau giữa
các trigger; regression hiệu năng; yêu cầu trong danh sách chưa có case pass. Chi tiết luật: [tham-chieu-testcase.md](tham-chieu-testcase.md#suite).
Quy trình cho QC: [huong-dan-qc.md](huong-dan-qc.md).

## 9. Thêm một service mới

Thêm tệp `testkit/services/<tên>.yaml`, không sửa code lõi. Các bước:

1. Image: tên/tag, hoặc `build` từ thư mục mã nguồn; nếu có failpoint, một image test (`test_tag`, build tag riêng).
2. Cổng HTTP, health check, metrics.
3. Kho dữ liệu dùng (migration, topic, index...), mock bên thứ 3 (kèm OpenAPI, phiên bản API, ngày và nguồn kiểm chứng).
4. Trigger (Kafka, RabbitMQ, Redis hoặc bảng DB poll; mỗi trigger khai báo nơi dead-letter để có `trigger.dlq`), entity (để viết `postgres.order.o1.status`...), failpoint, env (dùng hàm như
   `{{ pgdsn }}`, `{{ topic "orders" }}`, `{{ mock "payment" }}` để mỗi lần chạy có namespace riêng).
5. `./tk up --services <tên>`, viết một case, `./tk plan`, `./tk run`.
6. Thêm mục `conformance:` rồi `./tk gen --service <tên>` để có ngay bộ case chuẩn ([bo-case-chuan.md](bo-case-chuan.md)).

Tham chiếu đầy đủ: [tham-chieu-service.md](tham-chieu-service.md).

## 10. Trợ lý AI (tuỳ chọn)

Mặc định tắt (`ai.provider: none`): không gửi gì ra ngoài. Khi bật, chỉ dữ liệu **đã che** được gửi, và bị từ chối
gửi nếu còn sót secret/email/số thẻ. AI chỉ gợi ý, không quyết định kết quả.

```sh
./tk ai context out/<run_id> --task triage     # xem đúng nội dung sẽ gửi, không gửi
./tk ai triage out/<run_id>                    # gợi ý nguyên nhân cho case đỏ/flaky/yếu
./tk ai summary out/<run_id>                   # bản tóm tắt cho người ký
./tk ai draft --service order-worker --req-id REQ-300 --requirement "..."   # soạn nháp testcase (status draft)
```

Cấu hình model: [tham-chieu-cau-hinh.md](tham-chieu-cau-hinh.md#ai).

## 11. Tích hợp CI

Máy CI cần Docker (runner có quyền mount `docker.sock`). Ví dụ một job:

```sh
./tk doctor
./tk up --services order-worker --profile core,stores,mocks,observability,chaos
./tk gen --check                           # bộ case sinh ra phải khớp mô tả service (mã thoát 2 nếu lệch)
./tk run testkit/suites/release.yaml       # mã thoát quyết định job xanh/đỏ
# lưu out/*.zip, out/*/junit.xml làm artifact; JUnit cho trang kết quả test của CI
./tk down
```

Không có netem/NET_ADMIN trên runner thì để `gate.allow_skipped_capability: true` trong suite (các yêu cầu liên quan
được ghi "chưa kiểm được trên máy này"), hoặc chạy nhóm chaos trên runner có quyền.

## 12. Xử lý sự cố

| Triệu chứng | Nguyên nhân | Cách xử lý |
|---|---|---|
| `doctor`: nofile limit FAIL / Elasticsearch không lên (`error setting rlimit`) | Engine không cho `--ulimit nofile=65536` | `export TK_NOFILE_LIMIT=$(ulimit -Hn)` rồi `./tk up` lại |
| `up` báo cổng đã dùng | Cổng 5xxxx bị chiếm | Đổi `TK_PORT_*` trong môi trường (vd. `TK_PORT_GRAFANA=53001 ./tk up`) |
| Build image lỗi TLS / không tải được gói | Proxy công ty chặn TLS | `TESTKIT_BUILD_CA=/path/ca.pem`; kiểm tra `HTTPS_PROXY`, `NO_PROXY` |
| Case bị `SKIPPED (capability)` | Máy thiếu NET_ADMIN / `sch_netem` / docker.sock | Chạy trên máy có quyền; xem `./tk doctor` |
| Case đỏ loại `environment` | Hạ tầng chưa khoẻ, hết RAM/đĩa | `./tk status`, `docker stats`, `./tk down && ./tk up` |
| `no space left on device` | Đầy đĩa (image, `out/`) | Xoá run cũ trong `out/`, `docker system prune` (cẩn thận với dữ liệu khác) |
| Không có ảnh Grafana trong báo cáo | Chưa bật profile `observability` | `./tk up --profile core,stores,mocks,observability` |
| Perf báo "không có baseline cho môi trường ..." | Máy/phiên bản khác với baseline đã ghi | `./tk baseline record ...` trên stack rảnh |
| `verify`/`pack`/`report` từ chối gói | Có tệp bị sửa sau khi niêm phong | Chạy lại để có gói mới; không sửa tay trong `out/<run_id>/` |
| `pack` báo SECRET | Bằng chứng chứa giá trị secret đã khai báo | Sửa nơi thu thập/che dữ liệu, chạy lại; không gỡ kiểm tra |
| `ai ...` báo "refusing to send" | Payload sau khi che vẫn còn dữ liệu nhạy cảm | Xem `./tk ai context ...`; báo lỗi bộ che để sửa |
| Lỗi không rõ | — | Thêm `-v` để in mọi lệnh docker: `./tk -v run ...` |
