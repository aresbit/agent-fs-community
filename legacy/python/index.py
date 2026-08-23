#!/usr/bin/env python3
# Historical prototype only. The supported implementation is the Go CLI.
"""agent-fs 内容索引：重建 FTS5 全文索引（幂等维护工具）。

用法：
    python3 index.py [--db fs.db]

scan.py 会在导入时同步 files_fts；本工具用于文件内容/标签变更后，
从 files 表重建全文索引，保证搜索结果与实际内容一致。
"""
import os
import sqlite3
import sys


def db_path() -> str:
    return os.path.join(os.path.dirname(os.path.abspath(__file__)), "fs.db")


def rebuild(conn: sqlite3.Connection) -> int:
    """清空并重建 files_fts，从 files 表同步 name + content_head + 标签。"""
    with conn:
        conn.execute("DELETE FROM files_fts")
        # 聚合每个文件的标签成逗号串，与 name、content_head 一起入 FTS
        conn.execute(
            """
            INSERT INTO files_fts(name, tags, content_head)
            SELECT f.name,
                   (SELECT GROUP_CONCAT(tag, ',') FROM tags t WHERE t.ino = f.ino),
                   f.content_head
            FROM files f
            """
        )
        n = conn.execute("SELECT COUNT(*) FROM files_fts").fetchone()[0]
    return n


def main() -> int:
    db = db_path()
    if "--db" in sys.argv:
        db = sys.argv[sys.argv.index("--db") + 1]
    conn = sqlite3.connect(db)
    n = rebuild(conn)
    conn.close()
    print(f"全文索引重建完成：{n} 条 → {db}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
