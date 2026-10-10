# Bộ case chuẩn: mô tả nghiệp vụ một lần, TestKit sinh các case disruptive

Tài liệu này dành cho người viết mô tả service (dev, QC lead) và cho agent. Tham chiếu từng trường nằm ở
[tham-chieu-service.md](tham-chieu-service.md#conformance--nguồn-của-bộ-case-chuẩn); cú pháp case sinh ra nằm ở
[tham-chieu-testcase.md](tham-chieu-testcase.md).

## 1. Vấn đề và cách giải

Kiểm thử tự động một service chạy nền (nhận job từ Kafka, RabbitMQ, Redis hoặc một bảng DB được poll) thường đòi hỏi, cho
**từng** case: dựng dữ liệu, tính đầu ra kỳ vọng, giao job, cài lỗi, rồi viết assertion. Nhưng các kịch bản gây rối quan
trọng nhất lại **giống nhau ở mọi service**:

| Kịch bản | Câu hỏi nó trả lời |
|---|---|
| Job giao trùng | Hàng đợi chỉ đảm bảo *at-least-once*: giao 3 lần có làm hiệu ứng xảy ra 3 lần không? |
| Message độc | Một message hỏng có bị mất lặng lẽ, thử lại vô tận, hay chặn cả hàng đợi không? |
| Chết giữa chừng | Tiến trình chết đúng lúc đang xử lý: job có được giao lại và hoàn tất, có lặp lại việc đã làm không? |
| Lỗi phụ thuộc dưới tải | DB/cổng thanh toán chập chờn 5 giây dưới tải đều: có mất job, vào DLQ, xử lý trùng không; mất bao lâu để phục hồi? |
| Tới ngược thứ tự, phát lại muộn | Kết quả có phụ thuộc thứ tự tới, và job phát lại sau khi xong có lặp hiệu ứng không? |
| Tải ổn định | Độ trễ, thông lượng, tỉ lệ lỗi so với SLO và so với baseline cùng môi trường |

Bộ case chuẩn tách phần *riêng của service* (dữ liệu một job; thế nào là "xong"; hiệu ứng nào phải đúng một lần) khỏi phần
*chung* (các kịch bản trên). Bạn khai báo phần riêng **một lần** trong mục `conformance:` của mô tả service; `testkit gen`
sinh các case, cho từng trigger service có. Không ai phải tính tay giá trị kỳ vọng cho từng case: kỳ vọng là **bất biến**
("hiệu ứng đúng một lần", "không job nào mất", "không vào DLQ", "backlog về 0"), suy ra từ khai báo.

Những gì không đổi so với phần còn lại của TestKit:

- Case sinh ra là `status: draft`. Chỉ người chạy `testkit admit --approve --by <tên>` mới duyệt (sau khi cổng đột biến
  chứng minh case phát hiện được lỗi). Agent không được duyệt.
- Đúng/sai vẫn do assertion quyết định; bộ sinh không dùng model, không dùng đồng hồ: cùng mô tả cho cùng tệp, từng byte.
- Không `sleep`, không bên thứ 3 thật, mọi thứ khai báo trong tệp; thiếu quyền (netem...) thì case bị bỏ qua và báo cáo.

## 2. Bắt đầu nhanh

```sh
# 1. Thêm mục conformance vào testkit/services/<tên>.yaml (mục 4), rồi:
./tk lint                                         # mô tả service được kiểm trước khi chạm hạ tầng
./tk gen --service order-worker --list            # xem sẽ sinh gì, mẫu nào "n/a" và vì sao
./tk gen --service order-worker                   # ghi testkit/scenarios/generated/order-worker/*.yaml, tự lint

# 2. Chứng minh case phát hiện được lỗi (cần stack: ./tk up --services order-worker)
./tk run --mutations testkit/scenarios/generated/order-worker
./tk admit testkit/scenarios/generated/order-worker      # cổng đột biến; người duyệt thêm: --approve --by <tên>

# 3. Giữ cho bộ case khớp mô tả (CI)
./tk gen --check
```

Case hiệu năng (`steady-load-*`) cần baseline trước khi `admit`: `./tk baseline record <case>` trên máy rảnh.

## 3. Cách hoạt động

```
conformance (mô tả nghiệp vụ, 1 lần)        triggers (kafka | db-poll | rabbitmq | redis)
          │                                          │
          └────────────── testkit gen ───────────────┘
                              │   mẫu (duplicate, poison, crash, fault, order, load)
                              ▼
        testkit/scenarios/generated/<service>/*.yaml   (draft, tất định, có dấu "sinh bởi testkit gen")
                              │   run --mutations → admit → người --approve
                              ▼
                       case approved ──► suite release (only_approved)
```

Hai thứ làm cho một case viết một lần chạy được qua mọi công nghệ hàng đợi:

1. Trigger của TestKit (`trigger.enqueue`, `trigger.drain`, `load.start`) giao job qua từng công nghệ.
2. Hai check **trung lập**: `trigger.backlog` (job chưa xong: đang chờ + đang xử lý) và `trigger.dlq` (job service đã bỏ
   cuộc). Mỗi trigger cho hai con số này theo cách của nó (Kafka: lag và topic dead-letter; RabbitMQ: `ready + unacked` và
   DLQ; Redis: waiting + pending và DLQ; bảng DB: câu `drained` và câu `dead`). Bảng khai báo cần có:
   [tham-chieu-service.md](tham-chieu-service.md#triggers--cách-giao-job).

Vì vậy mỗi case sinh ra liệt kê `trigger: [kafka, db-poll, rabbitmq, redis]` và TestKit chạy nó qua từng trigger rồi
**so khớp kết quả** (trigger parity): hai công nghệ cho kết quả khác nhau là một phát hiện.

## 4. Viết mục `conformance`

Ví dụ thật: `testkit/services/order-worker.yaml` (cuối tệp). Từng bước:

### 4.1 Dữ liệu của một job: `given` và `shared`

```yaml
conformance:
  given:
    postgres.order:
      - { id: "{{ .key }}", customer_email: "cust-{{ .key }}+{{ .ns }}@shop.test", amount_cents: 1500, currency: USD, status: pending }
  shared:
    mock.payment: [201]
```

- `given` là **dữ liệu của đúng một job**, cùng cú pháp `given` của case (khoá là bước, giá trị là danh sách dòng).
  `{{ .key }}` là khoá duy nhất của job (`k1`, `k2`, ...): dùng nó làm id nghiệp vụ và trong địa chỉ mail để các job không
  đụng nhau. TestKit nhân khối này ra cho từng job của case.
- `{{ .ns }}` và `{{ .vars.x }}` **không** bị thay lúc sinh: chúng giữ nguyên để mỗi lần chạy có namespace riêng.
- `shared` là phần chỉ khai báo một lần mỗi case (kịch bản mock cổng thanh toán...). Một khoá không được ở cả hai nơi.
- Check không được dùng template (`{{ .ns }}` không hoạt động bên trong một check). Khi hiệu ứng gắn với một giá trị theo
  job như địa chỉ mail chứa `{{ .ns }}`, khai báo nó trong `vars` rồi tham chiếu bằng tên:

  ```yaml
  vars:
    "customer_{{ .key }}": "cust-{{ .key }}+{{ .ns }}@shop.test"      # thành vars của case, cho từng job
  effects:
    - { check: "mail.to(customer_{{ .key }}).count", eq: 1 }
  ```
  (địa chỉ trong `given` vẫn viết thẳng; `vars` chỉ là tên để check tham chiếu).
- Job giao qua trigger có `id = {{ .key }}`. Thêm trường vào job bằng `job: { field: ... }` nếu template trigger cần.

### 4.2 Thế nào là "xong": `done`

```yaml
  done:
    - { check: "postgres.order.{{ .key }}.status", eq: paid, why: "Đơn {{ .key }} được thanh toán" }
    - { check: "es.order.{{ .key }}.status", eq: paid }
    - { check: "postgres.order.count(status=paid)", per_job: 1, why: "Cả {{ .jobs }} đơn đều được thanh toán" }
```

Có hai loại mục, phân biệt bằng `{{ .key }}` trong `check`:

| Loại | Dấu hiệu | Dùng ở đâu |
|---|---|---|
| **Theo từng job** | `check` có `{{ .key }}` | Case nhỏ (giao trùng, message độc, crash, ngược thứ tự): sinh một assertion cho mỗi job |
| **Tổng** | `check` không có `{{ .key }}` | Mọi case, kể cả case lớn dưới tải. Kỳ vọng là toán tử bình thường (`eq: 3`) hoặc `per_job: N` (= N × số job) |

Case lớn (100 job dưới lỗi phụ thuộc) không thể kiểm từng dòng, nên **cần ít nhất một mục tổng có `per_job`** trong `done`:
nó vừa là assertion "không mất job" vừa là chỉ báo tải đã chảy trước khi gây lỗi. Thiếu thì `gen` báo rõ.

### 4.3 Hiệu ứng đúng một lần: `effects`

```yaml
  effects:
    - { check: mock.payment.succeeded, per_job: 1, why: "Trừ tiền đúng một lần cho mỗi đơn" }
    - { check: kafka.order-events.count, per_job: 1, why: "Đúng một sự kiện order.paid cho mỗi đơn" }
    - { check: "clickhouse.order_event.count(order_id={{ .key }})", eq: 1 }
```

Liệt kê mọi tác dụng phụ **không được lặp**: gọi bên thứ 3, gửi mail, phát sự kiện, ghi analytics, ghi audit... Đây là phần
quyết định chất lượng bộ case: một hiệu ứng không được liệt kê là một hiệu ứng không ai canh chừng khi job bị giao trùng.
Mỗi mục phải **có thể fail** (không viết kỳ vọng luôn đúng); cổng đột biến (mục 7) sẽ lộ mục nào không bắt được lỗi.

### 4.4 Chọn mẫu và cấu hình: `patterns`

Mẫu nào có khối trong `patterns` thì mới được sinh; khối rỗng `{}` dùng mặc định:

```yaml
  patterns:
    duplicate-delivery: { copies: 3 }
    poison-message:
      db-poll: { sql: "INSERT INTO jobs (job_key, order_id) VALUES ('poison-{{ .ns }}', '')" }
    crash-mid-job: { failpoint: crash_after_db_commit, log: "failpoint: crashing after db commit" }
    dependency-fault:
      faults:
        - { proxy: postgres, fault: down, for: 5s }
        - { proxy: payment, fault: reset_peer, for: 5s }
    out-of-order: {}
    steady-load:
      thresholds: { p95_ms: 1500, error_rate: 0.01, min_throughput: 15, max_dropped: 0 }
```

Mỗi mẫu có thể kèm `mutations` (mục 5, cuối mỗi mẫu). Chi tiết từng mẫu ở mục 6.

## 5. Service cần cung cấp gì

Checklist trước khi chạy `gen` (TestKit từ chối và nói rõ thiếu gì):

| Cần | Vì sao | Khai báo |
|---|---|---|
| Nơi dead-letter của **mỗi trigger được phủ** | Để `trigger.dlq` đếm được "job service đã bỏ cuộc" | Kafka: `triggers.kafka.dlq` (topic logic). RabbitMQ/Redis: `dlq` trong khai báo queue. DB: `triggers.db-poll.dead` (SQL đếm) |
| Định nghĩa "đã xử lý hết" | Để `trigger.backlog` chạy | DB: `triggers.db-poll.drained` (các trigger khác tự có) |
| Failpoint cho các cơ chế an toàn | Để cổng đột biến chứng minh case phát hiện được lỗi | `failpoints:` của service; mỗi `mutations[].failpoint` phải có ở đó |
| `perf.fixture` | Case lớn tạo dữ liệu cho `prefix+from..to` | `perf.fixture` (mẫu `dependency-fault`, `steady-load`) |
| `perf.completion` | Đo độ trễ đầu-cuối | `perf.completion` (mẫu `steady-load`) |
| `chaos.proxies` | Lỗi phụ thuộc đi qua Toxiproxy riêng của execution | `chaos.proxies.<tên>` (mẫu `dependency-fault`) |
| Service **idempotent** | Giao trùng / giao lại là điều bình thường của hàng đợi | Trong mã service. Bộ case chuẩn chính là thứ kiểm điều này |
| Thoát êm khi SIGTERM, log JSON có `run_id` | Bằng chứng và dọn dẹp | Xem [tham-chieu-service.md](tham-chieu-service.md#yêu-cầu-cho-service-để-test-tốt) |

Quy ước cho consumer của service (để các mẫu đo đúng):

- **Chỉ ack sau khi xử lý xong** (Kafka commit sau; RabbitMQ `ack` sau; Redis `XACK`/`LREM` sau). Ack trước rồi mới xử lý
  thì mẫu *chết giữa chừng* sẽ mất job, đúng như bộ case muốn phát hiện.
- Message không xử lý được thì **chuyển sang dead-letter đã khai báo** rồi ack bản gốc (RabbitMQ: `nack` không requeue để
  broker chuyển sang DLQ; Redis: ghi vào `dlq` rồi `XACK`).
- Lỗi tạm thời của phụ thuộc (DB chết) **không được** tiêu hao số lần thử hay dẫn đến dead-letter: đó là sự cố hạ tầng,
  không phải lỗi của job.

## 6. Sáu mẫu

Mỗi case sinh ra có `id` dạng `TC-STD-<SERVICE>-<MÃ>`, nằm ở `testkit/scenarios/generated/<service>/<tên mẫu>[-<hậu tố>].yaml`.
`expect_red` của đột biến nhận **tên nhóm** assertion ở bảng dưới (chọn mọi assertion của nhóm) hoặc **tiền tố check**
(`mock.payment.succeeded`) để chọn đúng assertion; nên dùng tiền tố khi chỉ một assertion trong nhóm là thứ lỗi đó làm hỏng.

| Mẫu | Mã / tệp | Trigger | Nhóm assertion |
|---|---|---|---|
| `duplicate-delivery` | `DUP` / `duplicate-delivery.yaml` | tất cả (một case) | `done`, `effects`, `backlog`, `dlq` |
| `poison-message` | `POISON-<TRIGGER>` / `poison-message-<trigger>.yaml` | mỗi trigger một case | `poison`, `done`, `effects`, `backlog` |
| `crash-mid-job` | `CRASH` / `crash-mid-job.yaml` | tất cả | `crash`, `restart`, `done`, `effects`, `backlog`, `dlq` |
| `dependency-fault` | `FAULT-<PROXY>-<LỖI>` / `dependency-fault-<proxy>-<lỗi>.yaml` | tất cả; mỗi lỗi một case | `done`, `effects`, `dlq`, `backlog`, `recovery`, `abort`, `load`, `reconcile` |
| `out-of-order` | `ORDER` / `out-of-order.yaml` | tất cả | `done`, `effects`, `backlog`, `dlq` |
| `steady-load` | `LOAD-<TRIGGER>` / `steady-load-<trigger>.yaml` | mỗi trigger một case | `dlq`, `reconcile` (+ ngưỡng SLO và baseline) |

### 6.1 `duplicate-delivery` — job giao trùng (rủi ro P0)

*Kiểm:* cùng một job giao `copies` lần (mặc định 3) cho `jobs` job (mặc định 2) → kết quả như giao một lần.
*Sinh:* dữ liệu cho từng job → `trigger.enqueue {id: k1, duplicate: 3}` cho từng job → assertion `done` và `effects` theo
từng job và tổng → `trigger.backlog = 0` → `trigger.dlq = 0`.
*Đột biến gợi ý:* bỏ kiểm tra "đã xử lý" (`skip_paid_check`) → `expect_red: [mock.payment.succeeded]`; bỏ chặn gửi mail trùng.
*Lưu ý:* nếu hiệu ứng của service tự chống trùng bằng khoá idempotency ở bên thứ 3, `mock.payment.succeeded` vẫn đếm đúng
số lần gọi thành công; hãy chọn check phản ánh đúng thứ bạn muốn canh.

### 6.2 `poison-message` — message độc (P1)

*Kiểm:* một message hỏng đứng **trước** một job hợp lệ. Message độc phải vào dead-letter (đúng một), không bị thử lại vô tận,
không chặn job hợp lệ phía sau.
*Sinh (mỗi trigger một case, vì cách đưa message độc khác nhau):* Kafka `kafka.produce` (cùng key với job hợp lệ để cùng
partition, nên message độc thật sự chặn nếu service xử lý sai); RabbitMQ `rabbitmq.publish`; Redis `redis.enqueue`; bảng DB
`postgres.exec` với câu `patterns.poison-message.db-poll.sql` (bảng job không có "thân message", nên bạn khai báo một dòng
job không dùng được; không khai báo thì mẫu được báo `n/a` cho db-poll). Thân hỏng mặc định `{not json` (đổi bằng `body`).
Sau đó `trigger.enqueue` job hợp lệ; assertion: `poison` (`trigger.dlq = 1`), `done`, `effects`, `backlog = 0`.
*Đột biến gợi ý:* `drop_dead_letters` (message không xử lý được bị bỏ thay vì chuyển DLQ) → `expect_red: [poison]`.

### 6.3 `crash-mid-job` — chết giữa chừng (P0)

*Kiểm:* tiến trình chết ở điểm `failpoint` (chạy `=once`, chỉ có trong image test), restart theo `restart`. Job phải được giao
lại và hoàn tất; việc đã làm trước khi chết không lặp.
*Cần:* `failpoint` (đã khai báo trong `failpoints`) và `log` — dòng log chứng minh tiến trình **thực sự** đã chết đúng chỗ
(không chứa dấu `)`); thiếu bằng chứng này thì case xanh có thể chỉ vì lỗi không bao giờ xảy ra.
*Sinh:* `failpoints: [<fp>=once]`, `sut.restart`, giao job; assertion `crash` (`sut.log(<log>).count = 1`), `restart`
(`sut.restarts = 1`), `done`, `effects`, `backlog = 0`, `dlq = 0`.
*Đột biến gợi ý:* bỏ kiểm tra "đã xử lý" → `expect_red: [mock.payment.succeeded]` (giao lại sau crash trừ tiền lần hai).

### 6.4 `dependency-fault` — lỗi phụ thuộc dưới tải (P0)

*Kiểm:* thí nghiệm chaos theo quy trình: tải nền cố định `rate` job/s (open model) cho `jobs` job → trạng thái ổn định → gây
lỗi qua Toxiproxy trong `for` → (chỉ khi `abort_backlog` > 0 và tồn đọng vượt ngưỡng: dừng khẩn cấp) → gỡ lỗi
→ đo thời gian về trạng thái ổn định (`trigger.backlog ≤ 5`).
*Cấu hình:* `faults[]` — `proxy` (trong `chaos.proxies`), `fault` (`down`, `latency`, `timeout`, `reset_peer`, `bandwidth`,
`slicer`), `with` (tham số, vd. `latency: 1500`), `for`, và `mutations` riêng của lỗi đó (một failpoint thường chỉ liên quan một
lỗi: `outage_is_failure` chỉ có nghĩa khi DB chết). Mỗi phần tử là một case.
*Assertion:* `done` và `effects` (chỉ loại **tổng**), `dlq = 0`, `backlog = 0`, `recovery` (`experiment.recovery_seconds ≤
max_recovery`), `abort` (chỉ có khi `abort_backlog` > 0: không chạm điều kiện dừng khẩn cấp), `load` (máy tạo tải không nghẽn), `reconcile` (mỗi mục
`reconcile:` của service có `mismatches = 0`).
*Quy tắc quan trọng nhất — sự cố phải dài hơn ngân sách thử lại của service.* Service thử mỗi job tối đa N lần với backoff;
một sự cố ngắn hơn tổng thời gian đó thì job vẫn được cứu và "không vào DLQ" đúng **kể cả khi service sai**: case xanh mà
không chứng minh gì, và đột biến chỉ bị bắt khi gặp may (đã gặp thật ở `order-worker`: sự cố 5 s với ngân sách ≈ 5 s, cùng một
đột biến lúc bị bắt lúc không, tuỳ pha thời gian). Đặt `for` > ngân sách thử lại (ở `order-worker`: 5 lần, backoff
0,5+1+1,5+2 s ≈ 5 s, nên dùng 12 s) và `jobs`/`rate` đủ để tải còn chảy suốt sự cố. Cổng đột biến là cách phát hiện vi phạm quy tắc này.
*Dừng khẩn cấp (`abort_backlog`):* chỉ dành cho blast radius (tồn đọng vượt ngưỡng). TestKit **không** dừng thí nghiệm khi có job
vào DLQ: đó chính là thuộc tính đang kiểm (assertion `dlq`); dừng sớm biến lỗi sản phẩm thành kết quả "bị huỷ/môi trường" và che verdict.
*Chạy một mình:* case chaos không chạy song song với case khác trên cùng stack.
*Đột biến gợi ý:* DB chết → `outage_is_failure` (`expect_red: [dlq]`); cổng thanh toán reset kết nối → `no_retry`.

### 6.5 `out-of-order` — ngược thứ tự và phát lại muộn (P1)

*Kiểm:* `jobs` job (mặc định 3) giao theo thứ tự **ngược** với lúc tạo, chờ xong hết, rồi phát lại **muộn** job đầu tiên.
Kết quả không được phụ thuộc thứ tự tới; job phát lại cho việc đã xong không được lặp hiệu ứng.
*Sinh:* `trigger.enqueue` k3, k2, k1 → `trigger.drain` → `trigger.enqueue` k1 → assertion `done`, `effects`, `backlog = 0`, `dlq = 0`.
*Giới hạn trung thực:* mẫu này kiểm tính độc lập thứ tự và chống phát lại. Nếu service có **ràng buộc thứ tự nghiệp vụ**
(sự kiện `shipped` đến trước `paid` phải bị hoãn hoặc từ chối) thì đó là case viết tay riêng của service.
*Đột biến gợi ý:* bỏ kiểm tra "đã xử lý" → `expect_red: [mock.payment.succeeded]`.

### 6.6 `steady-load` — tải ổn định (P1)

*Kiểm:* tải cố định `rate` job/s (mặc định 20) trong `duration` (20s) sau `warmup` (5s), `repeat` lần (3), qua từng trigger. Độ
trễ đầu-cuối = lúc job hoàn tất (`perf.completion`) − lúc máy tạo tải phát job. So sánh thống kê với baseline cùng môi trường
(khoá `<baseline>-<trigger>`, mặc định `std-<service>-<rate>rps-<trigger>`, hồi quy cho phép 10%).
*SLO là của đội service:* `thresholds` bắt buộc (`p95_ms`, `error_rate`, `min_throughput`, `max_dropped`). TestKit **không tự đặt
ngưỡng**.
*Trước khi `admit`:* ghi baseline trên máy rảnh: `./tk baseline record <case>`. Case hiệu năng chạy một mình; baseline theo
môi trường (dấu vân tay máy + phiên bản image).
*Assertion:* `dlq = 0` và `reconcile` (mọi kho khớp nhau dưới tải). Không có `done`/`effects` tổng vì số job phụ thuộc số lần lặp.

## 7. Chạy, kiểm bằng đột biến, duyệt

```sh
./tk run --mutations testkit/scenarios/generated/order-worker    # mọi case, mọi trigger, và mỗi đột biến
```

Đọc kết quả như case thường (`out/<run_id>/report.html`). Với bộ case chuẩn, vài tình huống đáng biết:

| Thấy | Nghĩa là | Việc cần làm |
|---|---|---|
| Đỏ ở `effects` khi không có đột biến | **Phát hiện thật**: service lặp một hiệu ứng khi job bị giao trùng/giao lại | Sửa service (có thể kèm cơ chế idempotency), chạy lại, kèm run id |
| Đỏ ở một trigger, xanh ở trigger khác (parity lệch) | Cách consumer của công nghệ đó xử lý khác (ack sớm, không chuyển DLQ, không giao lại) | Mở bằng chứng của trigger đỏ; sửa consumer |
| `assertion(s) could not be evaluated` (lỗi môi trường) | Check không đọc được (vd. thiếu khai báo `dlq`) | Sửa mô tả; không nới điều kiện |
| `mutation SURVIVED` | Case **không phát hiện được** lỗi đó: kỳ vọng quá lỏng, hoặc `expect_red` sai | Làm chặt `done`/`effects` trong mô tả, rồi `gen` lại. Chỉ sửa `expect_red` khi một cơ chế khác hợp lệ giữ assertion đó xanh — và ghi lý do |
| `mutation NOT EVALUATED` | Đột biến lỗi khi chạy (failpoint không có trong image test, case lỗi) | Sửa nguyên nhân |
| `n/a` trong `gen --list` | Mẫu không áp dụng được cho trigger đó, kèm lý do | Khai báo phần còn thiếu (vd. `db-poll.sql`) hoặc chấp nhận |

Sau đó: `./tk admit <thư mục>` (xanh ổn định `--stability` lần, đỏ dưới mọi đột biến); **người** xem lại và chạy
`./tk admit <tệp> --approve --by <tên>`. Không bao giờ nới toán tử, bỏ assertion, tăng `within` hay thêm retry để làm case xanh.

Đưa bộ đã duyệt vào bản release: thêm `../scenarios/generated/<service>` vào `cases:` của `testkit/suites/release.yaml`
(`only_approved: true` giữ các nháp ở ngoài cho đến khi được duyệt). Trước khi sang bản release cần thêm mã yêu cầu của bộ
(`requirement`, mặc định `REQ-STD`) vào `requirements:` của suite nếu muốn cổng đòi hỏi nó.

## 8. Vòng đời và an toàn khi sinh lại

- `gen` chỉ ghi/ghi đè tệp **nháp có dấu "sinh bởi testkit gen"**. Tệp đã `approved` (có khối `admission`) hoặc bị sửa tay
  không bao giờ bị đụng; nếu mô tả service đổi và cho ra case khác, `gen` báo **drift** (kèm lý do) thay vì ghi đè — xem lại,
  sinh sang thư mục khác (`--out`) để so sánh, rồi `admit` lại nếu khác có ý nghĩa.
- `gen --list`: không ghi gì, in kế hoạch (cả mẫu `n/a`). `gen --check`: không ghi, thoát mã 2 nếu có tệp thiếu / lỗi thời / mồ côi
  (đặt trong CI để mô tả và bộ case không lệch nhau).
- Đổi mô tả (thêm hiệu ứng vào `effects`) → `gen` cập nhật các nháp; các case đã duyệt vẫn là bản đã duyệt cho đến khi bạn
  chủ động thay. Gỡ một mẫu khỏi `patterns` → nháp cũ thành "stale"; `gen --prune` xoá chúng.
- Bộ sinh luôn lint các tệp vừa ghi; lỗi lint tức là mô tả hoặc bộ sinh sai.

## 9. Ví dụ: thêm một service mới dùng RabbitMQ

Service `invoice-mailer` đọc queue `invoices`, tạo hoá đơn trong Postgres rồi gửi một mail. Ngoài phần khai báo service
thông thường (image, store, mock SMTP, failpoint), thêm:

```yaml
stores:
  rabbitmq:
    queues: [{ name: invoices, dlq: invoices.dlq }]     # dlq: nơi dead-letter (bắt buộc cho bộ case chuẩn)
triggers:
  rabbitmq:
    queue: invoices
    key: "{{ .job.id }}"
    value: '{"job_id":"{{ .job.id }}","customer":"{{ .job.id }}"}'
conformance:
  given:
    postgres.customer: [{ id: "{{ .key }}", email: "{{ .key }}+{{ .ns }}@corp.test" }]
  vars:
    "customer_{{ .key }}": "{{ .key }}+{{ .ns }}@corp.test"
  done:
    - { check: "postgres.invoice.{{ .key }}.status", eq: sent }
    - { check: "postgres.invoice.count(status=sent)", per_job: 1, why: "Cả {{ .jobs }} hoá đơn được gửi" }
  effects:
    - { check: "mail.to(customer_{{ .key }}).count", eq: 1, why: "Mỗi khách nhận đúng một mail" }
  patterns:
    duplicate-delivery:
      mutations: [{ failpoint: skip_mail_guard, title: "Bỏ chặn gửi mail trùng", expect_red: [mail.to] }]
    poison-message: {}
    out-of-order: {}
```

Rồi `./tk gen --service invoice-mailer` cho 3 case × mọi trigger của service, mà không viết thêm dòng case nào.

## 10. Xử lý sự cố

| Thông báo của `gen` | Cách xử lý |
|---|---|
| `triggers.kafka.dlq: ... is required by the standard pack` | Khai báo `dlq` (tên logic topic dead-letter, đã có trong `stores.kafka.topics`) |
| `triggers.db-poll.dead ... is required` / `drained ... is required` | Khai báo hai câu SQL đếm job đã bỏ cuộc / chưa xong |
| `stores.rabbitmq.queues.<q>.dlq: required` (hoặc Redis) | Thêm `dlq` vào khai báo queue |
| `conformance.given: required` | Khai báo dữ liệu của một job |
| `conformance.done: ... at least one check` | Mô tả điều đúng khi job xong |
| `needs a total in conformance.done with per_job` | Thêm một mục tổng, ví dụ `{ check: "postgres.order.count(status=paid)", per_job: 1 }` |
| `mutation X: expect_red "Y" matches neither an assertion group ... nor the check` | Dùng tên nhóm ở bảng mục 6 hoặc tiền tố check có trong case |
| `failpoint "X" is not declared in failpoints` | Thêm failpoint vào `failpoints:` (và vào mã service) hoặc bỏ mutation |
| `needs perf.fixture` | Mẫu lớn cần SQL tạo dữ liệu cho `prefix+from..to` |
| `thresholds: the SLOs are the team's to set` | Điền ngưỡng SLO của đội |

## 11. Giới hạn

- Bộ case chuẩn **không thay** case nghiệp vụ viết tay (quy tắc giá, trạng thái, định dạng dữ liệu...). Nó phủ lớp
  "hành vi của hàng đợi và của tiến trình khi có sự cố", lớp mà mọi service chạy nền đều có.
- Chất lượng bộ case bằng chất lượng của `done` và `effects`: hiệu ứng không được liệt kê thì không được canh. Cổng đột biến
  là công cụ để biết điều đó.
- Mẫu `out-of-order` không biết ràng buộc thứ tự nghiệp vụ của service (xem 6.5). Mẫu `dependency-fault` dùng Toxiproxy cho các
  phụ thuộc đi qua mạng; chết cả container hạ tầng là thí nghiệm riêng (`chaos.container`, chạy một mình).
- Chưa có: sinh case cho scheduler không có trigger giao job, và cho SQS.

## 12. Đã kiểm chứng trên `order-worker`

<<KIEM-CHUNG>>
