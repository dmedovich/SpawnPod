# RC7: аудит и план реализации

Дата: 2026-10-12. Статус: выполняется по согласованному плану.
Основание: `SpawnPod-RC7-Controlled-Network-Access-RFC.md`.
Проверенная ревизия SpawnPod: `12c47ff`, `VERSION=v0.1.0-rc.6`.

## Текущее выполнение

Согласованное уточнение: выпуск RC7 готовится по результатам локальной приёмки;
проверку Fedora 44 / K3s v1.36.4 после выпуска выполняет пользователь.
Release notes должны явно отделять локально подтверждённые gates от серверных.
Отсутствие доступа к серверу не блокирует работу над локальным кандидатом.

- RC7-01: namespace исправлен в renderer; tests покрывают `ci` и custom
  namespace, YAML/inventory, switch/ready, widened/wrong-namespace policy,
  детерминизм и отсутствие дополнительного egress у isolated bundle.
  Добавлен direct test IPv4/IPv6 exact source policy; RFC уточнён по A5.
- RC7-02: локальный pinned Runner image найден; metadata подтверждает 13.1.0
  и digest `96af81b1b0cd928f3f135fb8dc021d47cff868c151a4fd28af971e892990ee7a`.
  `runner-source.example.yaml` проверен реальными двумя jobs: official Runner
  выбрал `default` и `source`; persisted records, Exec proxy env, verified HTTPS
  и отрицательные проверки внутри jobs успешны (`source-runner-02`, локально).
- RC7-03: SNI проверяется до dial, parser проверяет extension tails/duplicates/ECH,
  исправлена обработка нескольких Connection headers, добавлены лимиты sockets/
  requests, закрытие tunnels, общий HTTP deadline и CLI `--validate`.
  Race checks проходят; 10s fuzz-прогон выполнил 182907 inputs без ошибки.
  Lifecycle/timeout/overload regressions проходят; отдельный реальный 10s
  ClientHello timeout подтверждает отсутствие upstream dial.
  Отдельный subprocess test подтвердил SIGTERM exit и закрытие активного CONNECT.
- RC7-04/07: добавлены operator-owned Kubernetes example и draft
  `SOURCE-PROXY.md`; статический scratch runtime реально запущен в K3s.
- RC7-05: exact ClusterIP IPBlock на локальном K3s блокирует approved traffic
  после DNAT. Диагностическое разрешение backend IP восстановило HTTPS, но
  не использовано как production pin. Добавлен явный `sourceProxyPolicy:
  pod-selector`: AND exact Lease namespace + два фиксированных proxy labels,
  единственный TCP port; default `ip` сохранён. Renderer, startup и inventory
  используют одинаковую policy; unit tests покрывают оба режима и broadening.
  Gate `network-05-rotation` прошёл 39 реальных проверок: verified HTTPS,
  denied host/port/SNI, direct upstream/API/DNS/other Pod/metadata probes,
  isolated/unmanaged/foreign-namespace access, proxy egress и частичный
  label spoofing. Proxy Pod пересоздан (новый IP), Service IP сохранился,
  HTTPS повторно успешен. Все три owned namespace удалены, cleanup errors=[]
  (`.local/rc7-validation/network-05-rotation`, приватный локальный evidence).
  Повтор на новом rc.7 runtime (`network-rc7-02`) снова прошёл 39 assertions,
  cleanup errors=[]; все disposable namespaces отсутствуют.
  Metadata denial не имеет reachable positive control на этом локальном host;
  target-CNI проверка остаётся отдельным шагом пользователя.
- RC7-02/06: добавлен opt-in `SourceRunnerProfiles` и generator workflow.
  Первый реальный запуск остановлен отозванными историческими test credentials;
  отдельная временная регистрация позволила повтору пройти. Admission восстановлен,
  test namespaces удалены. Release gate теперь включает source-network и реальные
  два labels; baseline upgrade обновлён с RC4 до RC6. Изменённый GitHub workflow
  ещё не запускался. Локальный `source-runner-04` прошёл за 137s: private
  pinned actions/checkout с exact commit/shallow/origin assertions, OCI push/pull
  с проверкой digest, unauthenticated write denial и отказ при недоступном proxy.
  Package version, отдельный package owner, write:package token и Actions secret
  удалены. Test-only TLS sidecar имеет отдельное exact node:3001 egress правило;
  production public-only proxy boundary проверен независимым network gate.
  `source-runner-rc7-01` прошёл за 159s на новом RC7 runtime: отсутствие secret,
  token и user проверено после удаления. Отмена запущена только после persisted
  executing + LastCommandID; Forgejo UI принял её, запись cleaned, job Pod отсутствует.
  Для отмены использован временный repo writer, затем удалён с проверкой отсутствия.
  У Forgejo v15 нет GitHub-compatible API cancel; helper использует штатную
  origin-protected UI session, credentials которой никогда не передаются jobs.
- RC7-07: локальный `BundleUpgradeAndRecovery` прошёл за 298s: настоящий RC6
  renderer создал полный rollback bundle; RC6 → RC7 preview → intentional startup
  failure → полный RC6 rollback → повторный RC7 preview. Все четыре реальные
  Forgejo jobs успешны, Runner Secret UID/RV/data и записи окружений сохранены.
  Отдельно подтверждены восстановление исходного admission UID/spec и удаление
  test namespaces (`upgrade-01/upgrade-summary.json`). RC6 fixture использовал
  сохранённые release binaries с проверенными BINARY_SHA256SUMS и version/revision
  labels, но локально собранный OCI; это не проверка опубликованной подписи RC6.
- RC7-08: VERSION/spec/release notes переведены на rc.7. Dirty preview archives
  amd64/arm64 и amd64 OCI собраны; verify-release подтвердил checksums, version,
  revision/dirty/platform metadata и ELF architectures. Source и все три amd64
  binaries прошли govulncheck v1.8.0; полный short race suite прошёл.
  Чистая финальная сборка, публикация и signature/attestation verification открыты.
- Локальный K3s read-only discovery: Ready, v1.34.12+k3s1, amd64, Fedora 43.
  Это не целевой Fedora 44 / K3s v1.36.4 из RFC.
- DNS lookup вне sandbox подтвердил `git.dmedovich.com → 138.124.231.231`;
  verified HTTPS из Pod через proxy подтвердил reachability, не ownership.
- `make check` (full tests, vet, module verification, diff check) прошёл.
  Race-проверки proxy/CLI/adapter/config и ранее installation/admission/provider
  прошли. Kubernetes example прошёл server-side dry-run на локальном K3s.
  Дополнительно выполнен live CNI gate, описанный выше.

Находки ниже описывают исходное состояние аудита; прогресс указан выше.

Аудит охватывает исходники, release workflow, документацию, локальные
исторические отчёты и доступный исходник Runner. Кластер, DNS, registry
и опубликованные подписи в рамках этого аудита не проверялись.

## Что уже реализовано

| Область | Подтверждённое состояние | Источник |
| --- | --- | --- |
| Бинарник | Прокси собирается для amd64/arm64 с version, trimpath, CGO=0; входит в архив и BINARY_SHA256SUMS | `scripts/release.sh`, `scripts/verify-release.sh` |
| Runtime | Оба Dockerfile включают `/spawnpod-source-proxy`; scratch, USER 10001; стандартный entrypoint — `/spawnpod` | `deploy/release/{runtime,multiarch}.Dockerfile` |
| Публикация | Workflow публикует общий multiarch runtime, SBOM/provenance, cosign-подпись и attestations | `.github/workflows/release.yml` |
| Маршруты | 1–64 exact host:port, literal IP, запрет ряда специальных диапазонов, loopback только явно | `internal/sourceproxy/proxy.go` |
| HTTPS | CONNECT и проверка SNI первого полного ClientHello; TLS не расшифровывается | `internal/sourceproxy/proxy.go` |
| Конфигурация | source требует HTTP proxy URL с literal unicast IP/port; неиспользуемый sourceProxy запрещён; дополнительная policy учитывается в quota validation | `internal/config/config.go` |
| Команды | Адаптер заменяет четыре proxy env и очищает оба NO_PROXY при Exec source-профиля | `internal/adapter/forgejo/server.go` |
| Политика jobs | Exact IP /32 или /128 и TCP port; selector managed=true, network=source | `internal/provider/kubernetes/network_policy.go` |
| Renderer | Политика уже добавляется в `render-admission`, а через него — в `20-admission.yaml` bundle и inventory | `internal/admission/images.go`, `internal/installation/bundle.go` |
| Startup | Контроллер требует default-deny и exact spec source policy до обслуживания jobs | `internal/controller/controller.go` |
| Admission | Поддерживает source/isolated; запрещает PodSpec env, Secret volumes и произвольные entrypoints | `internal/admission/images.go` |

Локальный архив RC6 содержит release.json с revision
`1660c0eb37fbc0cbb1e12b47dc430c03d2f7e92f`, dirty=false, linux/amd64.
Это исторический артефакт другой ревизии; его наличие не подтверждает
публикацию текущего HEAD или воспроизводимость новых сборок.

## Находки и пробелы

### A1. Namespace source policy не попадает в YAML и inventory — блокер

`BuildSourceProxyPolicy` создаёт объект без Namespace.
`admission.Render` сериализует его без присвоения `cfg.Kubernetes.Namespace`.
Bundle переносит пустое поле в inventory, а inventory checker использует его
в Kubernetes GET. Применение YAML зависит от namespace kubectl/context;
startup ищет policy именно в workload namespace.

Исправление: задавать namespace на уровне renderer; проверить YAML,
inventory и switch/ready проверки для `ci` и нестандартного workload namespace.
Не добавлять второй экземпляр policy в `10-network.yaml`.
Текущее размещение в фазе POLICY допустимо: она предшествует запуску Controller.

### A2. Отсутствует Kubernetes deployment прокси — блокер

Есть только route YAML с `listen: 127.0.0.1:3128` и локальные systemd fixtures.
Нет готового ConfigMap/Deployment/Service и proxy-side ingress/egress policies.
Нет health endpoint, метрик, лимита активных соединений и отдельного CLI validate.
TCP probe проверит listener, но не валидность pin или доступность upstream.

Отдельный образ с отдельным именем ещё не собирается. Однако общий подписываемый
scratch runtime уже позволяет отдельный Deployment с
`command: ["/spawnpod-source-proxy"]`. RFC допускает проверенный эквивалент.
Рекомендация для RC7: использовать этот образ с explicit command и digest,
чтобы сохранить существующую цепочку сборки и подписания. Специализированный
образ нужен только при отдельном требовании к составу артефакта.

### A3. Два профиля нельзя получить простым переименованием labels — блокер

SpawnPod выбирает профиль через `Create.BackendOptions["profile"]`, иначе default.
В доступном локальном исходнике `/home/dmedovich/runner-main/runner`:

- `act/runner/run_context.go` передаёт options выбранного plugin в Create;
- `internal/pkg/labels/labels.go` отклоняет неизвестную query option `profile`;
- схема label выбирает ключ в `plugins`; несколько ключей могут указывать
  на один Unix socket с разными options.

Кандидат решения: сохранить plugin/label `spawnpod` с profile=default,
добавить plugin `spawnpod-source` с profile=source на тот же socket и label
`spawnpod-source:spawnpod-source://<source-image@sha256:...>`.
Workflow тогда использует `runs-on: spawnpod-source`.

Это вывод по доступному исходнику, не готовая инструкция: дерево Runner
без Git metadata, соответствие опубликованному v13.1.0 не подтверждено.
Нужен тест точного pinned Runner image с двумя labels, записью выбранных
профилей в records и проверкой сетевого поведения каждого job.

### A4. Тесты прокси покрывают базовые случаи, но не весь RFC — блокер

В `internal/sourceproxy/proxy_test.go` пять тестов: часть route validation,
HTTP exact route, успешный CONNECT, mismatch SNI и базовый ClientHello parser.
Нет fuzz tests, полной матрицы malformed/fragmented ClientHello, overload,
shutdown активных tunnels и граничных timeout сценариев.
В `network_policy_test.go` есть service-policy test; отдельного теста
`BuildSourceProxyPolicy` там нет. Bundle tests используют isolated config.

Особого внимания требуют:

- `clientHelloSNI` возвращает результат сразу после SNI extension, не проверяя
  последующие extensions; строгую обработку malformed/duplicate данных
  следует определить и закрепить тестами;
- CONNECT dial происходит до SNI validation: rejected SNI не передаёт TLS
  bytes, но уже создаёт TCP connection к разрешённому upstream;
- hijacked connections не отслеживаются main server при `server.Close()`;
  требуется определить завершение активных tunnels при SIGTERM;
- HTTP не имеет общего deadline на streaming body; активные connections
  не ограничены, CONNECT ограничен абсолютным сроком 30 минут;
- hop-by-hop обработка использует `Header.Get("Connection")`; проверить
  несколько строк Connection и удаление названных ими headers.

Это задачи проверки и усиления реализации; аудит не доказывает обход allowlist.

### A5. Формулировки RFC требуют уточнения

1. HTTP поддерживает любой явно allowlisted port, а не только 80:
   `lookup(authority, "80")` использует 80 лишь при отсутствии порта.
2. Host validation отклоняет mixed case, а не нормализует его.
3. Существующие service policies разрешают job → approved service.
   Поэтому утверждения «isolated не достигает других workloads» и
   «source достигает только proxy» требуют оговорки про services.
   Для первоначального RC7 acceptance держать services disabled;
   совместимость source + services проверить отдельно либо явно ограничить.
4. Запрет private upstream не является запретом всех cluster/node endpoints:
   публичные node/API адреса могут пройти route validation. Нужны
   operator review и точная proxy-side egress policy.
5. Startup сверяет обязательные policies, но не доказывает отсутствие других
   разрешающих policies. Policies складываются; acceptance должен проверять
   полный набор правил и фактический direct bypass.

### A6. Исторические checkout проверки есть; нового release gate пока нет

Ignored архив `.local/development/docs-archive/docs/implementation/35-*` и `36-*`
описывает успешные shell Git checkout и private Node actions/checkout на
локальном K3s v1.34.12, amd64, с host proxy и loopback TLS fixture.
Тест использовал временный Runner config с profile=source, затем восстановил его.

Это полезный опыт, но не доказательство двух labels одновременно,
containerized proxy, Service VIP enforcement, target K3s v1.36.4 или OCI push/pull.
Текущий upgrade CI собирает proxy binary, но не запускает полный RC7 network gate.

## Предлагаемая последовательность RC7

| Этап | Работа | Условие завершения |
| --- | --- | --- |
| RC7-01 | Исправить namespace policy; source renderer/inventory/check tests; уточнить RFC по A5 | Bundle детерминирован, policy в правильном namespace, isolated output без дополнительного egress |
| RC7-02 | Проверить два plugin aliases на точном Runner v13.1.0 image | Один Runner выполняет default/source jobs, records подтверждают profile, старый label работает |
| RC7-03 | Усилить proxy validation/parser/headers/connection lifecycle; добавить необходимое bounded resource control | Positive/negative/fuzz/race checks; malformed requests fail closed; shutdown предсказуем |
| RC7-04 | Добавить operator-owned Kubernetes assets, используя общий runtime image | Non-root, read-only rootfs, tokenless, explicit command/config, requests/limits, TCP probes, ingress/egress restrictions |
| RC7-05 | Проверить endpoint на disposable K3s и затем target CNI | Proxy works; DNS/direct upstream/другие endpoints denied; isolated и unauthorized pods не достигают proxy |
| RC7-06 | Включить воспроизводимый network acceptance в CI; проверить Forgejo checkout и OCI transport | Два profiles, реальные TLS requests, scoped disposable registry push/pull, cleanup/cancellation, RC6 regression |
| RC7-07 | Runbook установки, pin rotation, диагностика, upgrade/rollback; release notes | RC6 → RC7 → RC6 → RC7 проверено; tokens отсутствуют в bundles/logs; exact digests записаны |
| RC7-08 | VERSION, release artifacts и существующая signature/attestation цепочка | amd64/arm64 archives/images проверены; platform metadata и signatures подтверждены; опубликован checklist доказательств |

Зависимости: RC7-01/02/03 → RC7-04 → RC7-05 → RC7-06 → RC7-07/08.
Отдельно помечать build support arm64 и реально выполненный arm64 acceptance.

ClusterIP — кандидат, окончательный выбор endpoint после RC7-05.
Если фактический DNAT/CNI не исполняет существующий exact-IP contract,
нужен пересмотр endpoint/policy дизайна с повторной отрицательной проверкой.
Подмена Service VIP нестабильным Pod IP не является готовым решением.

Proxy resources предлагается держать отдельным операторским комплектом,
с независимым lifecycle, как требует RFC. Job-side policy остаётся в основном
bundle. В документации явно задать порядок: proxy policies/config/deployment,
endpoint gate, затем drained SpawnPod upgrade. При rollback сначала выключить
source scheduling и восстановить полный RC6 bundle; default-deny сохранить.

OCI gate проверяет транспорт готового маленького OCI artifact через выбранный
proxy-aware клиент. Production builder, Hugo, GitOps и Argo CD — последующие задачи.
Клиент registry gate также обязан поддерживать ограничения job seccomp/runtime.

## Проверки этого аудита

Успешно выполнено:

```sh
go test -short ./internal/sourceproxy ./internal/config \
  ./internal/adapter/forgejo ./internal/provider/kubernetes \
  ./internal/installation ./internal/controller \
  ./internal/admission ./internal/deployment
```

Первый запуск блокировался sandbox на TCP/Unix sockets; повтор вне sandbox
прошёл во всех восьми пакетах. Полный make check, race/fuzz и live acceptance
в этом аудите не выполнялись. Изменения реализации не вносились.
