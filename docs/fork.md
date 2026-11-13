# Жизнь форка

Форк `fUS1ONd/chill-remna-bot` (образ — строчными, `fus1ond`) живёт без апстрима: PR автору не отправляются,
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

- `fork-image.yml` — собирает и публикует образ, только по тегу `v*-f<цифра>…`.
- `test.yml`, `security.yml` — авторские, гоняются на любой push и PR.
- `docker.yml`, `pages.yml` — авторские, в форке выключены двумя способами:
  - один раз при заведении форка (и после пересоздания) выполнить:
    ```bash
    gh workflow disable build-and-push -R fUS1ONd/chill-remna-bot
    gh workflow disable pages -R fUS1ONd/chill-remna-bot
    ```
    GitHub отключает воркфлоу по пути файла на всех ветках. Без этого пуш
    зеркала `main` (там авторские файлы без условий) опубликовал бы `latest`
    и arm64, а сайт — на Pages форка;
  - на `fus1ond` их джобы стоят под условием
    `github.repository_owner == 'Mrvibecodic'` — страховка, если воркфлоу
    кто-то включит обратно.

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
затем `git push fork fus1ond`. Если слияние меняет `.github/workflows`, токену
для пуша нужен scope `workflow`.

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
