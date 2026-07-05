# Rushes

Rushes is a PRD-driven, chat-first local video editing agent. The backend starts
from a Python 3.12 scaffold with uv, Pydantic v2 contracts, strict typing,
formatting, linting, tests, and CI gates.

## Development

```bash
uv sync
uv run pytest
uv run ruff check
uv run ruff format --check
uv run mypy
```

## 运行 API

```bash
scripts/dev_api.sh
```

默认监听 `127.0.0.1:8000`，工作区为当前目录下的 `.rushes`。启动时会在 stdout/log 打印带一次性 token 的本地入口 URL。
