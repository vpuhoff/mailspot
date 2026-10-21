# Сборка и зависимости

[← README](../README.md)

Исходники используемых зависимостей включены в каталог `vendor/`, версии перечислены в `vendor/modules.txt` и `go.mod`. Локальная и Docker-сборка используют `-mod=vendor`. Предварительный `go mod download` не требуется.

## Локальная сборка без сети

Нужен установленный Go 1.26 или новее. Из корня проекта:

```sh
make build
```

Команда собирает два исполняемых файла для текущей ОС и архитектуры:

- `bin/mailspot` — SMTP-приёмник и веб-интерфейс.
- `bin/loadtest` — нагрузочный клиент.

Сборка отключает CGO, загрузку модулей и автоматическую загрузку Go toolchain. Зависимости берутся из `vendor`, а не из кеша модулей. При отсутствии `make` выполните:

```sh
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local CGO_ENABLED=0 \
  go build -mod=vendor -buildvcs=false -trimpath -ldflags='-s -w' \
  -o bin/mailspot ./cmd/mailspot

GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local CGO_ENABLED=0 \
  go build -mod=vendor -buildvcs=false -trimpath -ldflags='-s -w' \
  -o bin/loadtest ./cmd/loadtest
```

Для проверки проекта:

```sh
make test
make vet
```

Тесты используют локальные TCP-сокеты. Проверка гонок `-race` требует поддерживаемой платформы, включённого CGO и C-компилятора; для самой сборки приложения они не нужны.

## Docker

```sh
docker compose up -d --build
```

Dockerfile копирует `vendor` вместе с проектом и собирает приложение без скачивания Go-модулей. Проверка сборки с отключённой сетью для инструкций `RUN`:

```sh
docker build --network=none -t mailspot:local .
```

Базовые образы `golang:1.26-alpine` и `alpine:3.23` должны быть доступны Docker. Их первоначальная загрузка требует сети; `--network=none` относится к шагам сборки, а не к получению базовых образов.

## Обновление vendor

После намеренного изменения зависимостей выполните при доступной сети:

```sh
go mod tidy
make vendor
make build
make test
make vet
```

`go mod tidy` синхронизирует зависимости с импортами; версии меняйте явно через `go get module@version`. `go mod vendor` копирует необходимые пакеты из кеша модулей и при необходимости скачивает недостающие. Обновляйте `go.mod`, `go.sum` и `vendor/` вместе. Не редактируйте скопированные пакеты вручную.

В `vendor` сохраняются лицензионные файлы зависимостей, скопированные Go. Лицензия MIT самого Mailspot не заменяет лицензии этих библиотек.
