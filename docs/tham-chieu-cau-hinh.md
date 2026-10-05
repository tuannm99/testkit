# Tham chiếu: cấu hình, biến môi trường, lệnh

## Các tệp cấu hình

| Tệp | Nội dung |
|---|---|
| `testkit.yaml` | Cấu hình dự án: đường dẫn, tên project/network Docker, image do TestKit tự build, bàn giao QC (`qc`), trợ lý AI (`ai`) |
| `infra/compose/versions.env` | Phiên bản **ghim** của mọi image (không `latest`); ghi vào `manifest.json` mỗi lần chạy |
| `infra/compose/testkit.env` | Cổng trên máy (`TK_PORT_*`), tài khoản/mật khẩu **chỉ dùng cho test**, `TK_NOFILE_LIMIT` |
| `infra/compose/docker-compose.yml` | Hạ tầng theo profile (`core`, `stores`, `mocks`, `observability`, `chaos`) |
| `testkit/services/*.yaml` | Mô tả service — [tham-chieu-service.md](tham-chieu-service.md) |
| `testkit/suites/*.yaml` | Bộ chạy (release...) — [tham-chieu-testcase.md](tham-chieu-testcase.md#suite) |
| `testkit/baselines/<fingerprint>/*.json` | Baseline hiệu năng theo môi trường (commit vào repo) |

Biến môi trường của tiến trình luôn **ưu tiên hơn** giá trị trong các tệp `.env`
(vd. `POSTGRES_IMAGE=postgres:15.8-alpine ./tk up`, `TK_PORT_GRAFANA=53001 ./tk up`).

## `testkit.yaml`

```yaml
project: testkit                 # tên project Docker Compose (ghi đè: TESTKIT_PROJECT)
network: testkit_net             # mạng Docker dùng chung (ghi đè: TESTKIT_NETWORK)
compose: infra/compose/docker-compose.yml
env_files: [infra/compose/versions.env, infra/compose/testkit.env]
services_dir: testkit/services
scenarios_dir: testkit/scenarios
mocks_dir: testkit/mocks         # OpenAPI cho Mock Hub
out_dir: out                     # thư mục kết quả
baselines_dir: testkit/baselines
images: { ... }                  # image TestKit tự build: mockhub, runner, otel-collector, tkstats, toxiproxy, ui-runner

qc:
  tool: files                    # files (CSV + Markdown, mặc định) | zephyr-scale | "files, zephyr-scale"
  project_key: ORD               # zephyr-scale: mã project Jira
  api: https://api.zephyrscale.smartbear.com/v2
  token_env: ZEPHYR_TOKEN        # tên biến môi trường chứa token (không ghi token vào tệp)
  cycle_name: "{{ .suite }} · {{ .run_id }}"
  folder: /TestKit
  auto_create_test_cases: false

ai:
  provider: none                 # none | anthropic | openai-compatible | command
  model: claude-opus-5-5
  effort: high                   # anthropic: low | medium | high | xhigh | max
  # api_key_env: ANTHROPIC_API_KEY
  # base_url: http://localhost:11434/v1
  # command: [claude, -p]
  # max_tokens: 16000
  # timeout: 5m
```

### `ai`

| `provider` | Dùng khi | Cần |
|---|---|---|
| `none` (mặc định) | Không gửi gì ra ngoài. Lệnh `ai ...` ghi prompt đã che vào `ai/requests/`; dán vào model bất kỳ rồi đưa câu trả lời lại bằng `--response <tệp>` | — |
| `anthropic` | Claude qua SDK chính thức | Khoá ở `ANTHROPIC_API_KEY` (hoặc biến tên trong `api_key_env`); `model` (mặc định `claude-opus-5-5`), `effort` |
| `openai-compatible` | Model bất kỳ có endpoint `/chat/completions` (dịch vụ ngoài hoặc model nội bộ) | `base_url`, `model`, `api_key_env` nếu cần |
| `command` | Một chương trình đọc stdin, trả lời ra stdout (vd. `claude -p`, `ollama run <model>`) | `command` (danh sách tham số) |

Mọi provider đều nhận **bản đã che** và bị từ chối gửi nếu còn sót dữ liệu nhạy cảm; mỗi yêu cầu được ghi lại (đã che).

## Biến môi trường

| Biến | Tác dụng |
|---|---|
| `TESTKIT_BUILD_CA` | Tệp CA (PEM) cho các lần build image sau proxy chặn TLS |
| `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` | Chuyển vào các lần build image và vào container CLI (`./tk`) |
| `TK_NOFILE_LIMIT` | Giới hạn số file mở cho Elasticsearch/ClickHouse (mặc định 65536; hạ xuống `$(ulimit -Hn)` nếu engine từ chối) |
| `TK_PORT_*` | Cổng trên máy của từng thành phần (xem `testkit.env`) |
| `<TÊN>_IMAGE`, `TESTKIT_VERSION` | Ghi đè phiên bản image (vd. thử Postgres khác) |
| `TESTKIT_PROJECT`, `TESTKIT_NETWORK`, `TESTKIT_ROOT` | Ghi đè tên project, mạng, thư mục gốc |
| `DOCKER_SOCK` | Đường dẫn docker socket cho `./tk` (mặc định `/var/run/docker.sock`) |
| `TK_REBUILD=1` | Buộc `./tk` build lại image runner |
| `TK_QC_TOOL` | Ghi đè `qc.tool` cho một lệnh (vd. `TK_QC_TOOL=files,zephyr-scale ./tk qc export ...`) |
| `TK_QC_API` | Ghi đè URL API Zephyr Scale (vd. bản Data Center) |
| `ZEPHYR_TOKEN` (hoặc tên trong `qc.token_env`) | Token đẩy kết quả lên Zephyr Scale |
| `TK_AI_PROVIDER`, `TK_AI_MODEL`, `TK_AI_BASE_URL`, `TK_AI_COMMAND` | Ghi đè cấu hình `ai` cho một lệnh (vd. `TK_AI_PROVIDER=command TK_AI_COMMAND='claude -p' ./tk ai triage out/<run>`) |
| `ANTHROPIC_API_KEY` | Khoá API Claude (provider `anthropic`) |
| `TESTKIT_INTEGRATION=1` | Bật các contract test cần hạ tầng thật (`go test ./adapters/trigger/contract`) |

## Lệnh CLI

Dùng `./tk <lệnh>` (chỉ cần Docker) hoặc `./bin/testkit <lệnh>`. Tuỳ chọn chung: `--root <thư mục>`, `-v` (in mọi lệnh docker).

### Hạ tầng

| Lệnh | Tuỳ chọn |
|---|---|
| `doctor` | — Kiểm tra máy: Docker, quyền, cổng, RAM, đĩa, giới hạn file |
| `up` | `--services <svc,...>` (chỉ thứ service cần), `--profile core,stores,mocks,observability,chaos` (mặc định `core,stores,mocks`), `--build`, `--timeout 5m` |
| `status` | — |
| `down` | `--verify` (mặc định bật: báo lỗi nếu còn sót tài nguyên) |

### Testcase và chạy

| Lệnh | Tuỳ chọn |
|---|---|
| `steps` | — In từ vựng: bước, check, toán tử |
| `lint [tệp\|thư mục...]` | — |
| `plan [tệp\|thư mục...]` | — Chạy thử khô: in những gì sẽ tạo, gọi, kiểm tra |
| `run [tệp\|thư mục...]` hoặc `run <suite.yaml>` | `--mutations`, `--retries N`, `--parallel N`, `--trigger <tên>`, `--keep`, `--build`, `--no-observability`, `--run-id`, `--pack` |
| `admit <tệp...>` | `--stability N` (mặc định 2), `--approve --by <người>`, `--parallel`, `--build`, `--run-id` |
| `baseline record <case...>` | `--runs N` (mặc định 5) |
| `baseline show` | — |

### Bằng chứng và bàn giao

| Lệnh | Tác dụng |
|---|---|
| `verify out/<run>` | So sha256 với `manifest.json` |
| `report out/<run>` | Tạo lại báo cáo (từ chối nếu gói đã bị sửa) |
| `pack out/<run>` | Kiểm manifest + quét secret, tạo `out/<run>.zip` |
| `qc cases [tệp\|thư mục...]` | `out/qc/testcases.{csv,md}` |
| `qc export out/<run>` | Ghi lại tệp bàn giao QC của một run (vd. sau khi thêm `qc_key`) |
| `qc push out/<run>` | Đẩy lên Zephyr Scale (khi `qc.tool` có `zephyr-scale`) |
| `collect` | Xuất panel Grafana + log cho một khoảng thời gian: `--run-id`, `--service`, `--from`, `--to`, `--panels`, `--ns`, `--out` |
| `annotate` | Ghi annotation Grafana: `--run-id`, `--text`, `--time`, `--end`, `--tags` |

### Trợ lý AI

| Lệnh | Tác dụng |
|---|---|
| `ai context out/<run> --task triage\|summary` | In đúng nội dung (đã che) sẽ gửi; không gửi |
| `ai triage out/<run>` | Gợi ý nguyên nhân → `ai/triage.{json,md}`; `--response <tệp>` để dùng câu trả lời có sẵn |
| `ai summary out/<run>` | Tóm tắt cho người ký → `ai/summary.md`; `--response` |
| `ai draft --service <svc> --requirement "..."` | Soạn nháp testcase: `--requirement-file`, `--req-id`, `--id`, `--out`, `--repairs N`, `--response` |

## Script nghiệm thu

`scripts/acceptance/phase0.sh` … `phase7.sh` chạy lại kiểm chứng của từng giai đoạn (cần stack đang chạy, trừ phase 7 chạy
được không cần model). Hữu ích sau khi nâng cấp phiên bản image hoặc sửa lõi TestKit.
