# recipe-core

`core-api` — сервис 1 в Recipe AI. Находит рецепты, которые можно приготовить из
того, что лежит в холодильнике, и ставит в очередь их адаптацию локальной моделью.

```
recipe-web  →  core-api (этот репозиторий)  →  recipe-llm  →  модель
                  │
                  ├── PostgreSQL — корпус рецептов, пользователи, история
                  └── Redis — задачи адаптации, идемпотентность, rate limit
```

Это единственный процесс с доступом к Postgres. Соседние сервисы лежат рядом, в
`../recipe-web` и `../recipe-llm`.

## Что умеет

- **Поиск** (`POST /api/v1/cook/matches`) — синхронный SQL set-coverage по
  холодильнику и базовым продуктам. Модель не участвует, поэтому поиск работает и
  при лежащем `llm-service`. Рецепты с запрещёнными аллергенами и нелюбимыми
  продуктами отсекаются прямо в запросе.
- **Адаптация** (`POST /api/v1/cook/adapt`) — асинхронная. Сервис сразу отвечает
  `202` с id задачи, воркер в том же процессе вызывает `llm-service` и проверяет
  ответ модели, статус приходит через SSE на `GET /api/v1/cook/jobs/{id}`.
- **Аккаунт**: регистрация и вход (JWT, argon2id), предпочтения и аллергены,
  холодильник с автокомплитом по каталогу ингредиентов, история адаптаций с
  оценками.

Ответ модели не доверяется без проверки: аллергены сверяются по каноническим id
ингредиентов второй раз, после модели, а `base_recipe_id` обязан быть из набора
кандидатов. Дословный текст `directions` из корпуса через API не отдаётся никогда.

## API

Контракт — [`api/openapi.yaml`](api/openapi.yaml). Спецификация ведётся вручную,
`recipe-web` генерирует из неё типы, поэтому любое изменение HTTP-поведения
меняет этот файл в той же правке.

| Путь | Что делает |
|---|---|
| `GET /healthz`, `GET /readyz` | Liveness и readiness (Postgres и Redis, без `llm-service`) |
| `POST /api/v1/auth/{register,login,refresh}` | Аккаунт и токены |
| `GET`, `PUT /api/v1/me/prefs` | Диеты, кухни, аллергены, нелюбимые продукты |
| `GET`, `POST /api/v1/me/ingredients`, `DELETE …/{id}` | Холодильник |
| `GET /api/v1/ingredients?q=` | Автокомплит по каталогу |
| `POST /api/v1/cook/matches` | Поиск рецептов |
| `POST /api/v1/cook/adapt` | Задача адаптации, нужен заголовок `Idempotency-Key` |
| `GET /api/v1/cook/jobs/{id}` | Статус задачи: SSE или JSON-снимок |
| `GET /api/v1/me/history`, `POST …/{id}/rating` | История и оценки |

Ошибки приходят в едином конверте `{"error": {"code": "...", "message": "..."}}`.

## Запуск

Нужны Docker с Compose и Go 1.26.

```bash
make bootstrap   # сеть recipe-net и .env из .env.example
```

Перед запуском замени `JWT_SECRET` в `.env` (минимум 32 байта, например
`openssl rand -hex 32`).

```bash
make up          # postgres, redis, миграции и core-api с hot reload на :8080
```

Без корпуса поиск вернёт пустой список. Положи CSV из
[Kaggle 64k recipes](https://www.kaggle.com/datasets/prashantsingh001/recipes-dataset-64k-dishes)
в `data/recipes.csv` и загрузи его:

```bash
make ingest
```

Этапы загрузки и критерии её качества описаны в
[`tools/ingest/README.md`](tools/ingest/README.md).

Адаптация требует запущенного `recipe-llm` в той же сети `recipe-net`. Без него
поиск и всё остальное работают, а задачи адаптации завершаются ошибкой.

### Быстрая проверка

```bash
TOKEN=$(curl -s localhost:8080/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"me@example.com","password":"correct-horse"}' | jq -r .access_token)

curl -s localhost:8080/api/v1/me/ingredients -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"name":"eggs"}'

curl -s localhost:8080/api/v1/cook/matches -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"max_missing":2,"limit":5}'
```

### Запуск на хосте

`make run` поднимает `core-api` через `go run` против Postgres и Redis из Compose.
Он задаёт только адреса базы и Redis, `.env` не читается, поэтому `JWT_SECRET`
нужно экспортировать самому:

```bash
JWT_SECRET=$(openssl rand -hex 32) make run
```

## Конфигурация

Переменные окружения, значения по умолчанию — в [`.env.example`](.env.example).

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `DATABASE_URL`, `REDIS_URL` | — | Обязательны |
| `JWT_SECRET` | — | Обязателен, минимум 32 байта |
| `JWT_ACCESS_TTL`, `JWT_REFRESH_TTL` | `15m`, `720h` | Время жизни токенов |
| `LLM_SERVICE_URL` | `http://llm-service:9000` | Адрес `recipe-llm` |
| `LLM_SERVICE_TIMEOUT_SEC` | `180` | Таймаут вызова модели |
| `ADAPT_WORKERS`, `ADAPT_QUEUE_SIZE` | `2`, `64` | Пул воркеров и размер очереди |
| `JOB_TTL` | `1h` | Сколько задача живёт в Redis |
| `HTTP_PORT`, `LOG_LEVEL` | `8080`, `info` | |
| `CORS_ALLOWED_ORIGINS` | `http://localhost:5173` | Через запятую |

Контейнеру `core-api` адрес `LLM_SERVICE_URL` задаёт `docker-compose.yml`, значение
из `.env` на него не влияет. Число попыток адаптации и лимит запросов к auth
заданы в коде (`internal/platform/config`).

Задачи адаптации живут только в Redis и намеренно не долговечны: при остановке
сервиса задачи из очереди теряются, пользователь повторяет запрос.

## Разработка

```bash
make help              # все команды
make lint              # go vet и проверка gofmt
make test              # юнит-тесты
make test-integration  # интеграционные тесты, стек должен быть запущен
make migrate           # а также migrate-status и migrate-down
make psql              # консоль базы
make logs              # логи core-api
make down              # остановить, данные сохранить
make reset             # остановить и удалить том базы
```

Интеграционные тесты выполняют `DROP SCHEMA public CASCADE`, поэтому
`make test-integration` создаёт для них отдельную базу `recipes_test`. Никогда не
направляй `TEST_DATABASE_URL` на dev-базу: там загруженный корпус. Без этой
переменной тесты поднимают Postgres через testcontainers, а без Docker
пропускаются.

Схема меняется только миграциями goose в [`migrations/`](migrations/). Они вшиты
в образ `migrate`, и `make migrate` его не пересобирает: после новой миграции
сначала `make up` или `docker compose build migrate`.

### Устройство

```
cmd/api            сборка зависимостей и запуск HTTP-сервера
cmd/migrate        goose-миграции
internal/api       маршруты
internal/auth      регистрация, вход, JWT, rate limit
internal/user      предпочтения
internal/inventory холодильник
internal/catalog   каталог ингредиентов, автокомплит
internal/cook      поиск, очередь адаптации, воркер, валидатор, клиент llm-service
internal/recipehistory  история адаптаций и оценки
internal/platform  конфиг, логи, httpx, Postgres, Redis
migrations         схема БД
tools/ingest       загрузка корпуса (Python)
```

Пакеты предметной области устроены как handler → service → repo, SQL пишется
руками на pgx, интерфейсы объявляет потребитель.

Правила работы в репозитории и охраняемые инварианты (аллергены, изоляция
пользователей, контракт с `llm-service`) — в [`CLAUDE.md`](CLAUDE.md). Известные
проблемы и план работ — в [`TASKS.md`](TASKS.md).
