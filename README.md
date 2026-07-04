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
