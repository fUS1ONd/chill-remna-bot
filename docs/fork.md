# Жизнь форка

Форк `fUS1ONd/chill-remna-bot` живёт без апстрима: PR автору не отправляются,
доработки остаются здесь. Почему так — [ADR 0001](adr/0001-fork-without-upstream.md).

## Ветки и теги

| Что | Где |
|---|---|
| Работа форка | ветка `fus1ond` (ветка по умолчанию) |
| Зеркало автора | ветка `main`, только fast-forward к `main` автора, своих коммитов нет |
| Релиз форка | тег `v<версия автора>-f<N>` на `fus1ond` |
| Образ | `ghcr.io/fus1ond/chill-remna-bot:v<версия>-f<N>` и `:sha-<коротко>`, только linux/amd64, без `latest` |

`N` начинается с 1 для каждой новой версии автора и растёт с каждым
выпуском форка поверх неё: `v1.13.0-f1`, `v1.13.0-f2`, затем `v1.14.0-f1`.

## CI

- `fork-image.yml` — собирает и публикует образ, только по тегу `v*-f*`.
- `test.yml`, `security.yml` — авторские, гоняются на любой push и PR.
- `docker.yml`, `pages.yml` — авторские, в форке не работают: джобы стоят под
  условием `github.repository_owner == 'Mrvibecodic'`, а сами воркфлоу
  отключены в настройках форка (`gh workflow disable build-and-push`,
  `gh workflow disable pages`). Отключение нужно, потому что в `main` форка
  лежат авторские файлы без условия, и пуш зеркала иначе опубликовал бы
  `latest`.

## Подтянуть релиз автора

Ниже `origin` — репозиторий автора, `fork` — этот форк.

```bash
git fetch origin --tags
git push fork origin/main:main          # зеркало; только fast-forward
git switch fus1ond
git merge --no-ff v1.14.0               # merge-коммит от тега автора
```

Конфликты разрешаются в merge-коммите — так видно, что пришло от автора и где
оно задело доработки. В `.github/workflows` сохраняем условия форка из
раздела «CI». После слияния — gofmt, `go vet ./...`, `go test -race ./...`,
затем `git push fork fus1ond`.

Теги автора в форк не пушим (`git push --tags` нельзя): они не нужны, а
авторский `docker.yml` на них отреагировал бы, не будь он отключён.

## Выпустить `-f<N>`

```bash
git switch fus1ond
git tag v1.14.0-f1
git push fork v1.14.0-f1
```

CI соберёт образ с тегами `v1.14.0-f1` и `sha-<коротко>`. На сервере в compose
закрепляется конкретный тег, без `pull_policy: always` и без docker.sock.
