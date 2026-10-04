# Assumptions and defaults

Decisions taken without asking because they do not change the design. Each one
can be revisited; the line says where to change it.

| # | Phase | Assumption / default | Where to change |
|---|-------|----------------------|-----------------|
| A1 | 0 | The repository root is the TestKit project; `testkit.yaml` at the root is found by walking up from the working directory (or `TESTKIT_ROOT`). | `testkit.yaml` |
| A2 | 0 | Go module path `github.com/tuannm99/testkit/testkit` (TestKit) and `.../reference-worker`; tied together with `go.work`. Go 1.24 (`GOTOOLCHAIN=local`), dependencies chosen to stay compatible with it. | `go.work`, `*/go.mod` |
| A3 | 0 | The CLI drives Docker through the `docker` / `docker compose` CLI (not the Engine SDK): it is the one dependency every host has and every command is reproducible by copy-paste (`-v` prints them). Compose ≥ 2.20 is required for `up --wait`. | `core/infra` |
| A4 | 0 | Hosts with only Docker use `./tk`, which builds `testkit/runner` (CLI + docker CLI) and runs it with the docker socket and the project mounted at the same absolute path. Lifecycle commands (`up/down/doctor/status/lint/plan`) run outside the TestKit network so that `down` can remove it; the others join it. | `tk` |
| A5 | 0 | Every host port is published on `127.0.0.1` only, from `infra/compose/testkit.env`. A second independent stack on the same host = override `TESTKIT_PROJECT`, `TESTKIT_NETWORK`, `TK_PORT_*`. | `infra/compose/testkit.env` |
| A6 | 0 | Credentials in `testkit.env` are test-only and protect throw-away containers. Real secrets are never written in the repository; override through the environment. | `infra/compose/testkit.env` |
| A7 | 0 | Postgres runs with `fsync=off`, `synchronous_commit=off` and data on tmpfs: data is disposable and speed matters. Tests about database durability are out of scope of the default stack. | `docker-compose.yml` |
| A8 | 0 | Kafka: single KRaft broker (`apache/kafka`), `auto.create.topics.enable=false` (TestKit creates namespaced topics), `group.initial.rebalance.delay.ms=0` (faster tests). | `docker-compose.yml` |
| A9 | 0 | Mongo runs as a single-node replica set `rs0` so transactions/change streams work. From the host the CLI connects with `directConnection=true`. | `docker-compose.yml`, `core/config/endpoints.go` |
| A10 | 0 | `TK_NOFILE_LIMIT` (default 65536) is the RLIMIT_NOFILE of Elasticsearch/ClickHouse; `doctor` verifies the engine accepts it. Hosts that cap it (the CI sandbox caps at 20000) lower it. Single-node ES skips its bootstrap checks so a lower value only matters for very large loads. | `testkit.env` |
| A11 | 0 | Images TestKit builds (`mockhub`, `runner`, services under test) are tagged with `TESTKIT_VERSION` / the service descriptor tag; builder and base images are pinned in `versions.env` and passed as build args. Behind a TLS-intercepting proxy set `TESTKIT_BUILD_CA=<pem>`: it is mounted as a BuildKit secret, never baked into images. | `versions.env`, `core/infra/build.go` |
| A12 | 0 | `ghcr.io/shopify/toxiproxy` is the default Toxiproxy image. MinIO no longer publishes images on Docker Hub (pull fails); object storage for `pack` is decided in Phase 6. | `versions.env` |
| A13 | 0 | The reference worker is the system under test that proves TestKit end to end. Failpoints exist only in the image built with `-tags failpoint` (`test_tag` in the descriptor); `TK_FAILPOINTS="name;name=once"` activates them, `once` survives container restarts through a marker file. | `reference-worker/internal/failpoint` |
| A14 | 0 | Mock namespaces are path based (`/ns/{ns}/{mock}/...`): the service under test receives the namespaced base URL in its env, so isolation needs no cooperation from the service. | `adapters/mock/http` |
| A15 | 0 | Webhook signatures from the Mock Hub: header `X-Signature: sha256=<hex HMAC-SHA256(secret, body)>` (configurable header name). Other schemes can be added per mock. | `adapters/mock/http/hub.go` |
