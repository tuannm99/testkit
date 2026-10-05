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
| `redis` | `snapshot: true` (chụp các key dưới tiền tố của lần chạy) |

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
| `kafka` | `topic` (tên logic, được đổi theo namespace), `group`, `key`, `value` (template) |
| `db-poll` | `table`, `sql` (câu INSERT một job, template), `drained` (SQL trả số job chưa xong) |

Template của trigger có `{{ .job.id }}`, các trường khác của job (`{{ .job.order_id }}`...), `{{ .now }}`, `{{ .ns }}`,
`{{ .run_id }}`, `{{ .vars.* }}`. Lưu ý: hiện tại nếu case không truyền `order_id`, TestKit đặt `order_id = id` (giữ cho
service mẫu; service khác nên truyền trường mình cần một cách tường minh trong `job:`).

Khai báo cả hai trigger thì mỗi case chạy qua cả hai và kết quả phải giống nhau (so khớp trigger).

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
| `{{ redis }}`, `{{ keyprefix }}` | Redis và tiền tố key của lần chạy |
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
