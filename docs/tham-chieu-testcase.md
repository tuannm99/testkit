# Tham chiếu: tệp testcase

Testcase là một tệp YAML trong `testkit/scenarios/` (thư mục con tuỳ ý; `drafts/` cho bản nháp).
`./tk lint` kiểm tra mọi điều dưới đây; `./tk steps` in từ vựng bước/check đúng với phiên bản đang dùng.

## Trường cấp cao nhất

| Trường | Bắt buộc | Ý nghĩa |
|---|---|---|
| `id` | có | Mã duy nhất, vd. `TC-ORDER-017` |
| `title` | có | Tiêu đề cho QC |
| `requirement` | có | Mã yêu cầu (một chuỗi hoặc danh sách) — dùng cho ma trận truy vết và cổng release |
| `risk` | có | `P0` \| `P1` \| `P2` |
| `status` | có | `draft` \| `approved`. Chỉ `testkit admit --approve --by <người>` đổi thành `approved` |
| `owner` | | Người phụ trách case |
| `qc_key` | | Mã test case trong công cụ QC (khi đã import), xuất kèm kết quả |
| `service` | có | Tên service trong `testkit/services/` |
| `purpose` | có | Case chứng minh điều gì (tiếng Việt, cho QC) |
| `preconditions` | có | Danh sách trạng thái giả định trước các bước |
| `vars` | | Biến tự đặt, dùng lại trong case (`{{ .vars.customer }}`); có thể tham chiếu biến khác |
| `input` | có | Dữ liệu đầu vào (`input: {}` nếu không có); được render theo lần chạy và ghi vào báo cáo |
| `trigger` | | Đường giao job: `kafka`, `db-poll`, `rabbitmq`, `redis` hoặc danh sách (service nhận job bằng cách nào thì liệt kê cách đó; `--trigger <tên>` chỉ chạy các case có khai báo trigger đó). Case chạy một lần cho **mỗi** trigger và kết quả phải giống nhau |
| `given` | | Chuẩn bị nhanh (xem bên dưới) |
| `steps` | | Các bước theo thứ tự |
| `expect` | | Kỳ vọng — quyết định pass/fail |
| `within` | | Thời gian tối đa chờ kỳ vọng đúng (mặc định `30s`) |
| `evidence` | có | `grafana: [panel...]` (tên panel khai báo trong service), `snapshot: [bảng...]` (mặc định theo service), `logs: false` để tắt log. `evidence: {}` = mặc định |
| `failpoints` | | Bật lỗi trong service cho cả case (image test), vd. `[crash_after_db_commit=once]` |
| `sut` | | `restart` (chính sách restart Docker, vd. `on-failure:3`), `env` (biến môi trường thêm, có template), `replicas` (số instance) |
| `mutations` | | Phản chứng (xem bên dưới) |
| `chaos` | | `proxies: [...]` (phụ thuộc đi qua Toxiproxy), `max_recovery: 30s` |
| `perf` | | Case hiệu năng (xem bên dưới) |
| `tags` | | Nhãn tự do |
| `generated` | | Nguồn gốc khi do công cụ/AI soạn (`by`, `at`, `sources`) — do công cụ ghi |
| `admission` | | Do `testkit admit --approve` ghi; không sửa tay |

## Biến và template

Mọi chuỗi trong `vars`, `input`, `given`, `steps.with`, `sut.env`, `perf.env/target` là Go template, render cho từng lần chạy:

| Biến | Giá trị |
|---|---|
| `{{ .ns }}` | Namespace của lần chạy, vd. `tk_20261005ui1_1` — đưa vào email, mã định danh để không đụng lần chạy khác |
| `{{ .run_id }}`, `{{ .trigger }}`, `{{ .case }}` | Run, trigger đang chạy, mã case |
| `{{ .vars.<tên> }}`, `{{ .input.<tên> }}` | Biến và đầu vào của case |
| `{{ .sut.http }}`, `{{ .sut.host }}` | Địa chỉ service đang test trong mạng Docker (theo các cổng khai báo) |
| `{{ .mocks.<mock>.secret }}` | Secret chỉ-dùng-cho-test của mock (ký webhook trong script tải) |

Hàm: `upper`, `lower`, `json`. Một giá trị chỉ gồm đúng một tham chiếu (vd. `"{{ .input.order }}"`) giữ nguyên kiểu gốc (map, list, số).

## `given` — chuẩn bị nhanh

Các khoá chạy theo thứ tự: dữ liệu kho → script mock → giao job.

| Khoá | Ví dụ | Tương đương |
|---|---|---|
| `<kho>.<entity>` (kho: `postgres`, `mongo`, `es`, `clickhouse`, `redis`) | `postgres.order: ["{{ .input.order }}"]` | `<kho>.insert` các dòng/tài liệu vào entity |
| `mock.<tên>` | `mock.payment: [500, "429(retry-after=2)", 201]` | `mock.script`: chuỗi phản hồi theo thứ tự, phản hồi cuối lặp lại |
| `job` / `jobs` | `job: { id: o1, duplicate: 2 }` | `trigger.enqueue` qua trigger của lần chạy |

Cú pháp phản hồi mock: số (`201`), `"500*3"` (lặp 3 lần), `"429(retry-after=2)"`, `"201(delay=2s)"`, `reset`, `hang`, `close`,
hoặc map đầy đủ `{status: 200, body: {...}, body_text: "...", headers: {...}, retry_after: "2", delay: {fixed: 2s}, fault: reset}`.

## `steps` — các bước

```yaml
steps:
  - step: <tên bước>
    name: <nhãn hiện trong timeline, tuỳ chọn>
    with: { ...tham số... }
```

| Nhóm | Bước (tham số bắt buộc → [tuỳ chọn]) |
|---|---|
| Trigger | `trigger.enqueue` [id, duplicate, from] · `trigger.drain` [timeout] |
| Chờ / kiểm giữa chừng | `wait.until` (check + toán tử, `within`) · `assert` (`expect: [...]`, `within`) · `assert.during` (`for`, `expect: [...]`) — điều kiện phải đúng **liên tục** trong khoảng thời gian |
| Postgres | `postgres.insert` rows [entity, table] · `postgres.exec` sql [args] · `postgres.query` sql |
| Kafka | `kafka.produce` topic, value [key, headers, count] |
| RabbitMQ | `rabbitmq.publish` queue, body [count, message_id, headers] |
| Elasticsearch | `es.insert` entity, rows · `es.refresh` index · `es.block_writes` index [enabled] |
| ClickHouse | `clickhouse.exec` sql · `clickhouse.optimize` table [final] |
| Redis | `redis.set` key, value [ttl] · `redis.del` key · `redis.expire` key [ttl] · `redis.enqueue` queue, value [count, id] (đẩy vào hàng đợi stream/list đã khai báo) |
| Mongo | `mongo.insert` entity, rows |
| Mock HTTP/webhook | `mock.script` mock [responses, rules, method, path, operation] · `webhook.send` mock, url, body [sign: valid (mặc định)\|invalid\|none, header, repeat, delay, method, headers] |
| SMTP | `mail.script` behaviours: `ok`, `451@data`, `550@rcpt`, `disconnect@data`, `slow(2s)` (mỗi phiên SMTP một hành vi) |
| WebSocket/TCP | `socket.configure` mock [auto_ack, pong, faults: [{after_messages, action: close\|reset\|half_open\|drop_acks}], script, framing] |
| Service đang test | `sut.restart`, `sut.stop`, `sut.kill` [signal], `sut.start`, `sut.pause`, `sut.unpause`, `sut.wait_healthy` (đều [replica]) |
| Tải | `load.start` rate, from, to [id_prefix] · `load.wait` [timeout] · `load.stop` |
| Chaos (Toxiproxy) | `chaos.latency` proxy, latency [jitter] · `chaos.timeout` · `chaos.reset_peer` · `chaos.bandwidth` proxy, rate · `chaos.slicer` · `chaos.down` / `chaos.up` proxy · `chaos.clear` |
| Chaos (Docker) | `chaos.container` action: pause\|unpause\|stop\|start\|restart\|kill [target: sut hoặc tên container hạ tầng] · `chaos.network` action: disconnect\|connect · `chaos.stress` [resource: cpu\|memory\|disk] · `chaos.netem` [delay, jitter, loss] (cần quyền `netem`) |
| Thí nghiệm | `chaos.hold` for [abort_if: [kỳ vọng...]] · `chaos.recover` expect [within] |
| UI | `ui.run` spec [name, base_url, grep, env] |

Ghi chú:
- `trigger.drain` hết giờ không làm case lỗi: việc service chưa xử lý xong sẽ do kỳ vọng phán xét.
- `load.wait` / `load.stop` báo lỗi **môi trường** nếu máy tạo tải không giao được đủ job (không bao giờ đổ lỗi cho service).
- Lỗi trên container hạ tầng dùng chung, case perf và case có `load.start` tự chạy một mình, sau nhóm chạy song song.

## `expect` — kỳ vọng

```yaml
expect:
  - { id: A1, check: postgres.order.o1.status, eq: paid, why: "Đơn phải paid sau khi retry" }
  - { id: A2, check: mock.payment.calls, between: [2, 3], why: "..." }
  - { id: A3, check: experiment.recovery_seconds, lte: 30, why: "..." }
```

Mỗi kỳ vọng: `id` (duy nhất trong case), `check`, **một** toán tử với giá trị kỳ vọng (hoặc `op:` + `expected:`), `why`
(bắt buộc, hiện cho QC), `tolerance` (cho `approx`).

Cách đánh giá: TestKit đo lặp lại cho tới khi đúng hoặc hết `within` (không sleep cố định); sau đó chờ trigger xử lý hết
(drain) rồi **đo lại một lần cuối** — giá trị cuối quyết định. Một giá trị từng đúng rồi đổi (vd. có thêm một lần gọi thừa)
sẽ fail kèm cả hai thời điểm.

### Toán tử

| Toán tử | Đạt khi |
|---|---|
| `eq`, `ne` | bằng / khác |
| `gt`, `gte`, `lt`, `lte` | so sánh số |
| `between: [min, max]` | min ≤ giá trị ≤ max |
| `approx: x` + `tolerance: t` | \|giá trị − x\| ≤ t |
| `in: [...]`, `not_in: [...]` | thuộc / không thuộc danh sách |
| `contains`, `not_contains` | chuỗi/danh sách chứa phần tử |
| `matches: "regex"` | khớp biểu thức chính quy |
| `exists`, `not_exists` | có / không có giá trị (không cần giá trị kỳ vọng) |
| `empty`, `not_empty` | rỗng / không rỗng |
| `len_eq: n` | độ dài bằng n |

### Check — đọc gì để so sánh

Dạng chung: `<nguồn>.<đối tượng>[.<mã>].<thuộc tính>` hoặc `...count(<trường>=<giá trị>)`.

| Nguồn | Ví dụ |
|---|---|
| `postgres` | `postgres.order.o1.status`, `postgres.order.o1.exists`, `postgres.order.count(status=paid)` |
| `kafka` | `kafka.order-events.count(key=o1)`, `kafka.topic(orders.dlq).count`, `kafka.lag(order-worker)` |
| `trigger` | `trigger.backlog` (job chưa xong: đang chờ + đang xử lý), `trigger.dlq` (job service đã bỏ cuộc, ở nơi dead-letter đã khai báo). **Trung lập với công nghệ**: cùng một câu check đúng cho Kafka, db-poll, RabbitMQ và Redis, nên một case viết một lần chạy được qua mọi trigger. Khai báo cần có: bảng ở mục `triggers` của [tham-chieu-service.md](tham-chieu-service.md) |
| `rabbitmq` | `rabbitmq.<queue>.ready`, `.unacked`, `.depth`, `.consumers`, `.published`, `.acked`, `.redelivered`, `rabbitmq.<queue>.dlq.ready`, `.dlq.messages` |
| `es` | `es.order.o1.status`, `es.order.count(status=paid)` |
| `clickhouse` | `clickhouse.order_event.count(order_id=o1)`, `clickhouse.order_event.duplicates` |
| `mongo` | `mongo.audit.count(order_id=o1)` |
| `redis` | `redis.key(order:o1:status)`, `redis.ttl(lock:o1)`, `redis.count(order:*)`, `redis.queue(<tên>).depth\|waiting\|pending\|total\|dlq\|messages` (hàng đợi stream/list đã khai báo) |
| `mock` | `mock.payment.calls`, `mock.payment.calls(status=201)`, `.succeeded`, `.schema_errors`, `.idempotency_keys`, `.in_flight`, `mock.psp.webhooks(status=200)` |
| `mail` | `mail.to(customer).count`, `.subject`, `.text`, `mail.smtp.sessions`, `mail.smtp.delivered` (`customer` có thể là tên biến trong `vars`) |
| `socket` | `socket.partner.distinct(type=order.paid)`, `.duplicates`, `.connections` |
| `reconcile` | `reconcile.paid_orders.mismatches`, `reconcile.paid_orders.count(store=elasticsearch)` |
| `sut` | `sut.restarts`, `sut.running`, `sut.log(order paid).count`, `sut.metric(worker_poll_empty_total)` |
| `experiment` | `experiment.recovery_seconds`, `experiment.aborted`, `experiment.load.sent`, `.late`, `.errors` |
| `ui` | `ui.passed`, `ui.failed`, `ui.flaky`, `ui.tests`, `ui.errors`, `ui.test(<tên test>).status` |

Entity (`order`, `audit`...) là tên khai báo trong `entities:` của service. `mock.<x>.succeeded` đếm phản hồi 2xx thật sự
giao cho service (không tính phản hồi lặp lại do cùng Idempotency-Key).

## `mutations` — phản chứng

```yaml
mutations:
  - { id: M1, failpoint: no_retry, title: "Tắt retry", expect_red: [A1] }
```

Khi chạy với `--mutations` (và trong `admit`, bộ release), TestKit chạy thêm case trên image test với failpoint bật.
Case **phải đỏ**, và nếu có `expect_red` thì đúng các kỳ vọng đó phải đỏ. Nếu case vẫn xanh: "mutation sống sót" — case
không bắt được lỗi nó tuyên bố. Một lần chạy mutation bị lỗi môi trường là "chưa đánh giá", không bao giờ tính là đạt.

`failpoint` là tên khai báo trong `failpoints:` của service; `=once` chỉ kích hoạt lần đầu (vd. crash một lần).

## `perf` — hiệu năng

| Trường | Ý nghĩa |
|---|---|
| `kind` | `smoke` \| `load` \| `stress` \| `spike` \| `soak` (nhãn) |
| `executor` | `trigger` (giao job theo nhịp cố định; độ trễ = lúc xong trong DB − lúc gửi) \| `k6` (script HTTP) |
| `warmup` | Khoảng khởi động, không tính vào số đo |
| `stages` | `[{rate: <job hoặc req/giây>, duration: 20s}, ...]` — open model: nhịp tới không chậm lại khi service chậm |
| `repeat` | Số lần đo (≥ 3 để kiểm định thống kê có nghĩa) |
| `id_prefix` | Tiền tố mã job/đơn của lần đo |
| `thresholds` | SLO (luật cứng): `p50_ms`, `p95_ms`, `p99_ms`, `error_rate`, `min_throughput`, `max_dropped` |
| `baseline` | `key` (tên baseline), `max_regression` (mặc định `10%`), `metrics` (mặc định p95_ms, p99_ms, throughput) |
| `script`, `target`, `env` | Cho `k6`: script cạnh tệp case, URL đích (`{{ .sut.http }}`), biến môi trường cho script |

Executor `trigger` cần service khai báo `perf.fixture` và `perf.completion` (xem tham chiếu service).

## Suite

Tệp `kind: Suite` (vd. `testkit/suites/release.yaml`), chạy bằng `./tk run <suite.yaml>`:

| Trường | Ý nghĩa |
|---|---|
| `suite`, `title` | Tên suite (xuất hiện trong báo cáo, test cycle) |
| `cases` | Tệp/thư mục, đường dẫn tính từ tệp suite |
| `exclude` | Mã case bỏ ra |
| `only_approved` | Mặc định `true`: case `draft` bị bỏ qua và liệt kê trong báo cáo |
| `mutations`, `retries`, `parallel` | Như tuỳ chọn của `run` |
| `pack` | `true`: tự tạo `out/<run_id>.zip` |
| `requirements` | Danh sách yêu cầu release phải được phủ bởi case pass |
| `gate.allow_skipped_capability` | `true`: case bỏ qua vì máy thiếu quyền chỉ được liệt kê, không chặn release |
| `quarantine` | `[{case, reason, owner, deadline: YYYY-MM-DD, ticket}]` — flaky được cách ly tới hạn |

Cổng **GO** chỉ khi tất cả đều đạt:
1. có ít nhất một lần chạy;
2. không fail/error;
3. không flaky ngoài danh sách cách ly (hoặc đã quá hạn);
4. không bỏ qua case (trừ thiếu quyền khi được phép);
5. mọi mutation làm case đỏ (khi suite bật mutation);
6. các trigger cho cùng kết quả;
7. không regression hiệu năng;
8. mọi yêu cầu trong `requirements` được phủ bởi case pass.

Kết quả hiển thị ở đầu báo cáo và trong `qc/results.md`; mã thoát `0` = GO, `1` = NO-GO.
