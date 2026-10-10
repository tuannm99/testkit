# Tham chiếu: mô tả service (`testkit/services/<tên>.yaml`)

Thêm một service để test = thêm một tệp YAML, không sửa code lõi. Ví dụ đầy đủ: `testkit/services/order-worker.yaml`.
Mọi lệnh (`lint`, `plan`, `up --services`, `run`) đều kiểm tra tệp này trước khi chạm hạ tầng.

## Khung tệp

```yaml
apiVersion: testkit/v1
kind: Service
name: order-worker               # chữ thường, số, dấu gạch ngang
description: ...
owner: team-payments
image: { ... }
ports: { http: 8080 }
health: { port: http, path: /healthz, timeout: 60s }
metrics: { port: http, path: /metrics }
stores: { ... }
mocks: { ... }
triggers: { ... }
entities: { ... }
failpoints: { ... }
env: { ... }
perf: { ... }                     # tuỳ chọn
conformance: { ... }              # tuỳ chọn: nguồn của bộ case chuẩn (testkit gen)
chaos: { proxies: { ... } }       # tuỳ chọn
reconcile: { ... }                # tuỳ chọn
panels: { ... }                   # tuỳ chọn
requires: [docker.sock]           # quyền đặc biệt service cần (nếu có)
```

Đường dẫn trong tệp tính từ thư mục chứa tệp.

## `image`

| Trường | Ý nghĩa |
|---|---|
| `name`, `tag` | Image chạy bình thường. Tag phải ghim cụ thể, **không** dùng `latest` |
| `test_tag` | Image test có failpoint (dùng cho mutation và case crash). Không khai báo thì case dùng failpoint/mutation không chạy được |
| `build.context`, `build.dockerfile` | Build image từ mã nguồn khi chưa có hoặc khi mã nguồn đổi (nhãn `testkit.source_hash`) |
| `build.args`, `build.test_args` | Build arg cho image thường / image test (vd. `test_args: { GO_TAGS: failpoint }`) |

## `ports`, `health`, `metrics`

`ports` đặt tên cho cổng (`http: 8080`); tên được dùng trong `health.port`, `metrics.port` và biến `{{ .sut.<tên> }}` của testcase.
`health` (đường dẫn trả 2xx khi sẵn sàng, `timeout` chờ khởi động). `metrics` (Prometheus) để lấy số liệu và ảnh Grafana.

## `stores` — kho dữ liệu

Chỉ khai báo kho service thực sự dùng; `up --services` chỉ khởi động những kho này.
Mỗi lần chạy case có kho riêng (database/index/topic/tiền tố key riêng) và được xoá khi xong.

| Kho | Trường |
|---|---|
| `postgres` | `migrations` (thư mục `*.sql`, áp theo tên), `snapshot` (bảng chụp vào bằng chứng sau mỗi case) |
| `kafka` | `topics: [{name, partitions}]`, `groups` (consumer group của service, để đo lag) |
| `elasticsearch` | `indices: { <tên>: { mappings: tệp.json } }` (replica luôn 0) |
| `clickhouse` | `migrations`, `snapshot` |
| `mongo` | `collections`, `snapshot` |
| `redis` | `snapshot: true` (chụp các key dưới tiền tố của lần chạy); `queues: [{name, kind: stream\|list, group, processing, dlq}]` — hàng đợi service tiêu thụ. `stream`: consumer group `group` (TestKit tạo từ đầu stream, service chấp nhận `BUSYGROUP`); `list`: mẫu tin cậy `BLMOVE` sang `processing` (mặc định `<name>:processing`), xoá khỏi đó sau khi xử lý. Khoá thật là `{{ rkey "<name>" }}` (có tiền tố namespace) |
| `rabbitmq` | `queues: [{name, dlq}]` — mỗi lần chạy một **vhost** riêng (= namespace, xoá khi xong); TestKit khai báo queue (và `dlq` qua dead-letter của broker), service **không** khai báo lại với tham số khác (dùng passive declare) và nên `nack` không requeue để vào DLQ |

## `mocks` — bên thứ 3

Không bao giờ gọi bên thứ 3 thật trong test. Mỗi bên thứ 3 là một mock trong Mock Hub:

| Trường | Ý nghĩa |
|---|---|
| `kind` | `http` (service gọi ra), `webhook` (bên thứ 3 gọi vào service), `smtp` (gửi mail), `socket` (WebSocket/TCP) |
| `openapi` | Tệp OpenAPI (nằm dưới `testkit/mocks/`); request của service được kiểm theo hợp đồng → `mock.<x>.schema_errors` |
| `operation` | operationId mặc định cho phản hồi được script |
| `api_version`, `verified_at` | Bắt buộc với http/webhook: phiên bản API thật mà mock bắt chước, ngày kiểm chứng gần nhất |
| `verified_against` | Bắt buộc với http/webhook: `sandbox` (đã đối chiếu sandbox của nhà cung cấp) hoặc `docs` (tự fake theo tài liệu — báo cáo ghi là rủi ro còn lại) |
| `secret` | Secret **chỉ dùng cho test** để ký webhook; đưa vào service qua `{{ mocksecret "<tên>" }}` |
| `protocol` | Với `socket`: `ws` hoặc `tcp` |

## `triggers` — cách giao job

| Trigger | Trường |
|---|---|
| `kafka` | `topic` (tên logic, được đổi theo namespace), `group`, `key`, `value` (template), `dlq` (tên logic của topic dead-letter, khai báo trong `stores.kafka.topics`; cần cho `trigger.dlq`) |
| `rabbitmq` | `queue` (đã khai báo trong `stores.rabbitmq`), `value` (template thân tin), `key` (template message id) |
| `redis` | `queue` (đã khai báo trong `stores.redis.queues`), `value` (template, vào trường `payload` của stream hoặc phần tử list), `key` (template, trường `id`) |
| `db-poll` | `table`, `sql` (câu INSERT một job, template), `drained` (SQL trả số job chưa xong; là `trigger.backlog`), `dead` (SQL trả số job service đã bỏ cuộc; là `trigger.dlq`) |

Template của trigger có `{{ .job.id }}`, các trường khác của job (`{{ .job.order_id }}`...), `{{ .now }}`, `{{ .ns }}`,
`{{ .run_id }}`, `{{ .vars.* }}`. Lưu ý: hiện tại nếu case không truyền `order_id`, TestKit đặt `order_id = id` (giữ cho
service mẫu; service khác nên truyền trường mình cần một cách tường minh trong `job:`).

Mỗi trigger cho biết **hai con số** để case viết một lần dùng cho mọi công nghệ (check trung lập `trigger.backlog` và
`trigger.dlq`, xem [tham-chieu-testcase.md](tham-chieu-testcase.md)):

| Trigger | `trigger.backlog` (chưa xong = đang chờ + đang xử lý) | `trigger.dlq` (service đã bỏ cuộc) | Khai báo cần có |
|---|---|---|---|
| `kafka` | lag của consumer group | số record trong topic dead-letter | `triggers.kafka.dlq` |
| `db-poll` | kết quả câu `drained` | kết quả câu `dead` | `drained`, `dead` |
| `rabbitmq` | `ready` + `unacked` của queue | `ready` của `dlq` trong khai báo queue | `stores.rabbitmq.queues[].dlq` |
| `redis` | waiting + pending | số phần tử của `dlq` trong khai báo queue | `stores.redis.queues[].dlq` |

Khai báo nhiều trigger thì mỗi case chạy qua từng trigger và kết quả phải giống nhau (so khớp trigger). Drain của mỗi loại
đều chính xác, không đoán: RabbitMQ `ready = 0` và `unacked = 0` ổn định ≥ 1,3 giây (số liệu quản trị trễ ~0,5 giây);
Redis stream không còn entry sau `last-delivered-id` của group và danh sách pending rỗng; Redis list rỗng cả queue lẫn
`processing`; Kafka lag = 0; db-poll theo câu SQL `drained`.

## `entities`

Đặt tên nghiệp vụ cho dữ liệu để testcase viết `postgres.order.o1.status` thay vì SQL:

```yaml
entities:
  order:
    postgres: { table: orders, key: id }
    elasticsearch: { table: orders, key: _id }
  audit:
    mongo: { table: audit, key: _id }
```

Một entity có thể nằm ở nhiều kho (`postgres`, `elasticsearch`, `clickhouse`, `mongo`).

## `failpoints`

`tên: ý nghĩa khi lỗi này được cài`. Failpoint là chỗ trong **mã nguồn service** có thể bật để cố tình làm hỏng một cơ chế
(chỉ có trong image test). TestKit bật bằng biến môi trường `TK_FAILPOINTS="tên;tên=once"`.
Service mẫu dùng build tag `failpoint` (`reference-worker/internal/failpoint`): bản release không chứa đoạn mã này.
Mỗi testcase nên có ít nhất một mutation dùng failpoint; thiếu failpoint phù hợp thì dev cần thêm.

## `env` — biến môi trường của service

Giá trị là template, render cho từng lần chạy để service nối đúng namespace:

| Hàm / biến | Giá trị |
|---|---|
| `{{ .NS }}`, `{{ .RunID }}` | Namespace, run id |
| `{{ pgdsn }}`, `{{ database }}`, `{{ pguser }}`, `{{ pgpass }}` | Postgres của lần chạy |
| `{{ brokers }}`, `{{ topic "x" }}`, `{{ group "x" }}` | Kafka, tên topic/group theo namespace |
| `{{ es }}`, `{{ index "x" }}` | Elasticsearch, tên index theo namespace |
| `{{ ch }}`, `{{ chuser }}`, `{{ chpass }}` | ClickHouse (database: `{{ database }}`) |
| `{{ mongo }}` | Mongo (database: `{{ database }}`) |
| `{{ redis }}`, `{{ keyprefix }}`, `{{ rkey "x" }}` | Redis, tiền tố key của lần chạy, khoá đầy đủ của hàng đợi `x` |
| `{{ amqp }}`, `{{ amqpuser }}`, `{{ amqppass }}`, `{{ vhost }}` | RabbitMQ: URL `amqp://user:pass@host/<vhost>` của lần chạy, tài khoản test, vhost |
| `{{ mock "x" }}` | URL gốc của mock HTTP `x` cho lần chạy |
| `{{ mocksocket "x" }}` | URL WebSocket của mock `x` |
| `{{ smtpmock }}` | SMTP của Mock Hub (có chèn lỗi); địa chỉ người gửi nên chứa `{{ .NS }}` để mock biết lần chạy |
| `{{ smtp }}` | SMTP của Mailpit (không chèn lỗi) |
| `{{ mocksecret "x" }}` | Secret test của mock `x` |
| `{{ otlp }}` | OTLP endpoint (trace/metric gửi về OTel Collector) |

Mật khẩu xuất hiện trong bằng chứng (danh sách env của container) luôn được che `[REDACTED]`.

## `perf` — cho executor `trigger`

| Trường | Ý nghĩa |
|---|---|
| `fixture` | SQL tạo các đối tượng nghiệp vụ cho các job `{{ .prefix }}{{ .from }}..{{ .to }}` |
| `completion` | SQL trả `(id, thời điểm xong theo epoch giây)` cho các job có tiền tố `{{ .prefix }}` — độ trễ đầu-cuối = lúc xong − lúc gửi |

## `chaos.proxies`

Các phụ thuộc mà testcase có thể cho đi qua Toxiproxy (`chaos.proxies: [payment]` trong case):

```yaml
chaos:
  proxies:
    payment:
      upstream: mockhub:8081
      env: { PAYMENT_URL: "http://{{ proxy }}/ns/{{ .NS }}/payment" }
```

`{{ proxy }}` là địa chỉ proxy cấp riêng cho lần chạy; `env` ghi đè biến của service khi case dùng proxy đó.

## `reconcile` — đối soát giữa các kho

```yaml
reconcile:
  paid_orders:
    description: Mỗi đơn đã thanh toán có mặt đúng một lần ở mọi kho
    sources:
      - { store: postgres, sql: "SELECT id FROM orders WHERE status = 'paid'" }
      - { store: elasticsearch, index: orders, field: order_id }
      - { store: clickhouse, sql: "SELECT order_id FROM order_events WHERE type = 'order.paid'" }
      - { store: mongo, collection: audit, filter: { type: order.paid }, field: order_id }
```

Cần ít nhất 2 nguồn. Testcase dùng `reconcile.paid_orders.mismatches`, `...count(store=elasticsearch)`; báo cáo đối soát được
ghi vào bằng chứng mỗi lần chạy.

## `panels` — ảnh Grafana làm bằng chứng

`tên: { dashboard: <uid>, panel_id: <số> }`. Testcase chọn panel trong `evidence.grafana`. TestKit chụp ảnh panel trong đúng
khoảng thời gian của case (lọc theo run/namespace) và lưu cả số liệu thô (JSON/CSV) lấy bằng chính truy vấn của panel.
Dashboard đặt trong `infra/grafana/dashboards/` (được nạp sẵn khi bật profile `observability`).

## Yêu cầu cho service để test tốt

- Đọc mọi địa chỉ/kết nối từ biến môi trường (không viết cứng).
- Có health check; xuất metrics Prometheus và log JSON có `run_id` (lấy từ env) để lọc bằng chứng.
- Có failpoint cho các cơ chế an toàn quan trọng (retry, idempotency, khoá, chống trùng), chỉ biên dịch vào image test.
- Thoát êm khi nhận SIGTERM (hoàn tất việc đang làm).

## `conformance` — nguồn của bộ case chuẩn

Mô tả **nghiệp vụ của service một lần**; `testkit gen` sinh từ đó các case chuẩn (giao trùng, message độc, chết giữa chừng,
lỗi phụ thuộc, tới ngược thứ tự, tải ổn định) cho mọi trigger. Hướng dẫn đầy đủ, ví dụ và cách đọc kết quả:
[bo-case-chuan.md](bo-case-chuan.md). Mục này là tham chiếu các trường.

```yaml
conformance:
  owner: qc-lead                  # người sở hữu ghi vào case sinh ra (mặc định: owner của service)
  requirement: REQ-STD            # mã yêu cầu ghi vào case (mặc định REQ-STD)
  within: 45s                     # thời gian tối đa của case nhỏ (mặc định 45s)
  evidence: [jobs_total, ...]     # panel Grafana đính vào mọi case
  triggers: [kafka, db-poll]      # chỉ phủ các trigger này (mặc định: mọi trigger của service)

  given:                          # dữ liệu nghiệp vụ của MỘT job; {{ .key }} = k1, k2... (duy nhất mỗi job)
    postgres.order:
      - { id: "{{ .key }}", customer_email: "cust-{{ .key }}+{{ .ns }}@shop.test", amount_cents: 1500, currency: USD, status: pending }
  shared:                         # cho một lần mỗi case (kịch bản mock...), không nhân theo job
    mock.payment: [201]
  vars:                           # tên cho giá trị theo từng job mà check không viết trực tiếp được (tuỳ chọn)
    "customer_{{ .key }}": "cust-{{ .key }}+{{ .ns }}@shop.test"
  job: { }                        # trường thêm vào job giao qua trigger (.job.<trường>), tuỳ chọn

  done:                           # điều đúng khi một job đã hoàn tất
    - { check: "postgres.order.{{ .key }}.status", eq: paid, why: "..." }          # theo từng job (có {{ .key }})
    - { check: "postgres.order.count(status=paid)", per_job: 1, why: "..." }       # tổng: kỳ vọng = 1 × số job
  effects:                        # hiệu ứng phụ phải xảy ra ĐÚNG MỘT LẦN
    - { check: mock.payment.succeeded, per_job: 1 }

  patterns:                       # mẫu nào có khối ở đây thì mới được sinh ({} = dùng mặc định)
    duplicate-delivery: { copies: 3, jobs: 2, mutations: [...] }
    poison-message:     { body: "{not json", db-poll: { sql: "..." }, mutations: [...] }
    crash-mid-job:      { failpoint: crash_after_db_commit, log: "crashing after db commit", restart: "on-failure:3", mutations: [...] }
    dependency-fault:   { jobs: 100, rate: 10, max_recovery: 30s, abort_backlog: 0, faults: [{ proxy: postgres, fault: down, for: 5s, mutations: [...] }] }
    out-of-order:       { jobs: 3, mutations: [...] }
    steady-load:        { rate: 20, duration: 20s, warmup: 5s, repeat: 3, thresholds: { p95_ms: 1500, ... }, baseline: <tiền tố khoá> }
```

| Trường | Ý nghĩa |
|---|---|
| `given` | Bắt buộc. Mỗi khoá là một bước `given` như trong case (`postgres.order`, `es.order`...), giá trị là **danh sách dòng**; TestKit nhân ra cho từng job và thay `{{ .key }}`. Các template chạy lúc thực thi (`{{ .ns }}`, `{{ .vars.x }}`) được giữ nguyên |
| `vars` | Tên → giá trị, cả hai là template theo `{{ .key }}`; thành `vars:` của case cho từng job. Dùng khi check không viết thẳng được giá trị theo job, vd. `mail.to(customer_{{ .key }}).count` (đối số của `mail.to` là tên biến hoặc địa chỉ; `{{ .ns }}` không dùng được trực tiếp trong check) |
| `shared` | Cùng cú pháp, chỉ khai một lần trong case (không nhân theo job). Một khoá không được nằm ở cả `given` lẫn `shared` |
| `done`, `effects` | Danh sách `{ check, why, <toán tử> \| per_job }`. Check có `{{ .key }}` được kiểm cho **từng job** của case nhỏ; check không có `{{ .key }}` là **tổng** và dùng ở mọi case (kể cả case lớn dưới tải, nơi không thể kiểm từng dòng). `per_job: N` = kỳ vọng bằng N × số job. `{{ .jobs }}` trong `why` được thay bằng số job. Cần ít nhất một mục `done` |
| `patterns.<mẫu>.triggers` | Giới hạn trigger cho riêng mẫu đó |
| `patterns.<mẫu>.mutations` | `{ failpoint, title, expect_red }`: lỗi cố ý phải làm case đỏ. `failpoint` phải có trong `failpoints`. `expect_red` là **tên nhóm assertion** của mẫu (xem từng mẫu trong [bo-case-chuan.md](bo-case-chuan.md)) hoặc **tiền tố của một check** (`mock.payment.succeeded`) để chọn đúng assertion |
| `duplicate-delivery.copies / jobs` | Số lần giao mỗi job (mặc định 3) / số job (mặc định 2) |
| `poison-message.body` | Thân message hỏng (mặc định `{not json`). `db-poll.sql`: bảng job không có "thân message" nên khai báo câu INSERT một job không dùng được; thiếu thì mẫu được báo `n/a` cho db-poll |
| `crash-mid-job.failpoint / log / restart` | Failpoint làm service chết (chạy `=once`), dòng log chứng minh đã chết (không chứa dấu `)`), chính sách restart (mặc định `on-failure:3`) |
| `dependency-fault.faults[]` | `proxy` (có trong `chaos.proxies`), `fault` (`down`, `latency`, `timeout`, `reset_peer`, `bandwidth`, `slicer` = bước `chaos.<fault>`), `with` (tham số bước, vd. `latency: 1500`), `for` (thời gian giữ lỗi, mặc định 5s), `mutations` (riêng cho lỗi này). Mỗi phần tử là một case. Cần `perf.fixture` |
| `dependency-fault.jobs / rate / max_recovery / abort_backlog` | Số job (100), tốc độ job/s (10), thời gian phục hồi tối đa (30s), ngưỡng tồn đọng làm dừng thí nghiệm khẩn cấp (0 = không dùng) |
| `steady-load.*` | `thresholds` **bắt buộc** (SLO do đội service đặt: `p95_ms`, `error_rate`, `min_throughput`, `max_dropped`); cần `perf.fixture` và `perf.completion` |

Điều kiện để sinh được bộ case (TestKit từ chối và nói rõ thiếu gì): mỗi trigger được phủ phải khai báo nơi dead-letter
(bảng ở mục `triggers`), `done` có ít nhất một mục, `failpoint` trong mọi `mutations` phải khai báo trong `failpoints`.
