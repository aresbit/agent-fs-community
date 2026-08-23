#!/usr/bin/env python3
# Historical prototype only. The supported implementation is the Go CLI.
"""agent-fs 目录扫描器：遍历目录树，把文件元数据导入 SQLite。

用法：
    python3 scan.py [目录] [--db fs.db] [--tag 标签]

设计：agent 要的是「数据语义」而非「字节」。扫描器把每个文件变成
files 表里的一行（路径/大小/类型/mtime/内容预览），后续 agent 用 SQL
查询这些行，而不是 readdir + stat 逐个遍历。
"""
import os
import sqlite3
import sys
import time

CONTENT_HEAD_LIMIT = 8192  # 内容预览最多存 8KB


def init_db(conn: sqlite3.Connection) -> None:
    """执行 schema.sql 建表。"""
    schema = os.path.join(os.path.dirname(os.path.abspath(__file__)), "schema.sql")
    with open(schema, encoding="utf-8") as f:
        conn.executescript(f.read())


def content_head(path: str, size: int) -> str:
    """提取文件内容预览：小文件全文、大文件前 8KB；只存可读文本。"""
    if size == 0:
        return ""
    if size > CONTENT_HEAD_LIMIT:
        # 大文件只读前 8KB（后续可接对象存储，此处只存预览）
        with open(path, "rb") as f:
            raw = f.read(CONTENT_HEAD_LIMIT)
    else:
        with open(path, "rb") as f:
            raw = f.read()
    try:
        return raw.decode("utf-8")
    except UnicodeDecodeError:
        try:
            return raw.decode("latin-1")  # 总能解码，保留可读文本
        except Exception:
            return ""


def scan(conn: sqlite3.Connection, root: str, tags: list[str]) -> int:
    """遍历 root，把每个文件/目录写入 files 表，返回导入条数。"""
    root = os.path.abspath(root)
    # 记录 path -> ino 映射，用于填 parent_ino
    ino_of: dict[str, int] = {}
    count = 0
    pending: list[tuple[int, str, str, str, int, int, int, str]] = []

    for dirpath, dirnames, filenames in os.walk(root):
        # 目录本身
        rel = os.path.relpath(dirpath, root)
        path = root if rel == "." else os.path.join(root, rel)
        parent = os.path.dirname(path)
        st = os.stat(path)
        kind = "dir"
        # 先收集，父目录的 ino 需要先插入
        pending.append((parent, os.path.basename(path), path, kind, 0, st.st_mode, int(st.st_mtime), ""))

        # 文件
        for name in filenames:
            fp = os.path.join(dirpath, name)
            try:
                st = os.stat(fp)
            except OSError:
                continue
            kind = "symlink" if os.path.islink(fp) else "file"
            size = st.st_size
            head = content_head(fp, size) if kind == "file" else ""
            pending.append((dirpath, name, fp, kind, size, st.st_mode, int(st.st_mtime), head))

    with conn:  # 事务：全部导入成功才提交
        for parent, name, path, kind, size, mode, mtime, head in pending:
            parent_ino = ino_of.get(parent, 0)
            cur = conn.execute(
                """INSERT INTO files(parent_ino, name, path, kind, size, mode, mtime, content_head)
                   VALUES(?,?,?,?,?,?,?,?)""",
                (parent_ino, name, path, kind, size, mode, mtime, head),
            )
            ino = cur.lastrowid
            ino_of[path] = ino
            count += 1
            # 同步 FTS 全文索引（name + content_head）
            conn.execute(
                "INSERT INTO files_fts(name, tags, content_head) VALUES(?,?,?)",
                (name, "", head),
            )
            # 给根目录打标签（tags 参数）
            if tags and path == root:
                for t in tags:
                    conn.execute("INSERT OR IGNORE INTO tags(ino, tag) VALUES(?,?)", (ino, t))
    return count


def main() -> int:
    args = sys.argv[1:]
    root = "."
    db_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "fs.db")
    tags: list[str] = []
    i = 0
    while i < len(args):
        if args[i] == "--db" and i + 1 < len(args):
            db_path = args[i + 1]; i += 2
        elif args[i] == "--tag" and i + 1 < len(args):
            tags.append(args[i + 1]); i += 2
        else:
            root = args[i]; i += 1

    conn = sqlite3.connect(db_path)
    init_db(conn)
    t0 = time.time()
    n = scan(conn, root, tags)
    conn.close()
    print(f"扫描完成：{n} 个文件/目录 → {db_path}（{time.time()-t0:.2f}s）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
