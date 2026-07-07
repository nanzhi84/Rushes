# packages/storage — SQLite 持久化

- **`schema.py` 是数据库结构的单一定义**（SQLAlchemy Core `MetaData` + `Table`，对应 PRD §3.2）。`schema.create_all(connection)` 在 API 与 worker 启动时都会跑（`checkfirst`，可重复）。写事务统一用 `db.begin_immediate`（SQLite 立即拿写锁，配合 job claim 的多 worker 安全）。
- **`repositories/` 是薄封装**：每张表一个 `*Repository`，只做行的增删查改。JSON 列（`*_json`、`document_json` 等）的编解码集中在 `repositories/_json.py`（`load_json` / `dump_json`），别在别处手搓 `json.dumps`。
- **`data_migrations.py` 是启动期幂等修复**，不是 alembic 版本迁移：`apply_data_migrations(connection)` 每次启动都跑。单级草稿模型改版为删库重建（无存量），历史迁移已清空，当前恒为 **no-op 骨架**。以后要修历史库时，按文件内的样板注释——靠**存在性守卫**做到可重复（`sqlite_master` 查表、`PRAGMA table_info` 判列后再 `ALTER`/回填/删），遵循「先检查存在与否、已是目标态就 no-op」模式。
- `object_store.py` 是内容寻址的对象存储（`put_bytes` → `object_hash`），代理/缩略图等二进制走它。`workspace_paths.py` 定义工作区目录布局（db、objects、tmp 等）。
