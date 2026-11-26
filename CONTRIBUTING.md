# Contributing

## Develop

You need Go (the version in `go.mod`), Docker, and [golangci-lint](https://golangci-lint.run) v2.

```bash
go test -race ./...
golangci-lint run ./...
docker build -t massdriver-automations:dev .
docker run --rm massdriver-automations:dev list
```

To run an automation against a real organization, see [Run locally](README.md#run-locally).

## Add an automation

1. Make a package under `automations/<name>`. The name is lowercase with hyphens, and it is also the command
   (`run <name>`).
2. Implement `automation.Automation`, and `automation.Router` if it serves more routes. See
   [Write an automation](README.md#write-an-automation).
3. Export a `Definition` with the event types it uses, and add it to `automations/registry.go`.
4. Add `config.example.yaml` with a comment on every key. The tests load each example, so it must stay valid.
5. Add a `README.md` with what it does, its config, its environment variables, the
   `mass event destination create` command, and a `docker run` line.
6. Add the automation to the table in the top-level README and to both bundles in `bundles/`. Its
   settings form comes from `scripts/bundle-params.py`: add it there and run the script, which rewrites both
   `massdriver.yaml` files. Then run `mass bundle build` in each bundle.
7. Test it with `automation.Handler` and `automation.SignRequest` against `httptest` fakes. No test calls a real
   service.

## Rules

- Environment variables hold credentials and deployment wiring only. Behavior goes in the config file.
- Never log or put in a span an environment variable, a config value, a token, a request body, or a full URL.
- Logs are structured: short lowercase messages, `snake_case` keys, `info` when the automation changed
  something and `debug` when it skipped something. No emoji in logs or in messages to Slack, Teams, or GitHub.
- Return an error from `Handle` only when Massdriver should send the event again.
- Massdriver delivers an event at least once. Handling the same event twice must not repeat side effects.

## Pull requests

Keep one change in one pull request, and add a label (`enhancement`, `bug`, `documentation`, `security`, or
`debt`). The label puts the change in the right section of the release notes. CI must pass.

## Release

A maintainer pushes a `vX.Y.Z` tag. The publish workflow builds the images, tags them `X.Y.Z` and `X.Y`, attests
them, and creates the GitHub release with notes from the merged pull requests.
