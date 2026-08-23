#!/usr/bin/env python3
# Historical prototype only. The supported implementation is the Go CLI.
"""agent-fs 写操作映射：创建/删除/重命名/标签 → SQL 事务，元数据与真实文件一致。

用法：
    python3 ops.py tag   <路径> <标签>       # 打标签（纯元数据）
    python3 ops.py untag <路径> <标签>       # 去标签
    python3 ops.py rename <路径> <新名字>    # 重命名（文件 + 元数据）
    python3 ops.py rm    <路径>              # 删除（文件 + 元数据）

每个写操作都在单个 SQLite 事务内完成，保证元数据的一致性；涉及真实文件
的操作（rename/rm）同时更新文件系统与元数据。
"""
import os
import sqlite3
import sys


def db_path() -> str:
    return os.path.join(os.path.dirname(os.path.abspath(__file__)), "fs.db")


def connect() -> sqlite3.Connection:
    return sqlite3.connect(db_path())


def get_ino(conn: sqlite3.Connection, path: str) -> int:
    r = conn.execute("SELECT ino FROM files WHERE path=?", (path,)).fetchone()
    return r[0] if r else 0


def do_tag(conn: sqlite3.Connection, path: str, t: str) -> None:
    ino = get_ino(conn, path)
    if not ino:
        print(f"文件不存在: {path}", file=sys.stderr); return
    with conn:
        conn.execute("INSERT OR IGNORE INTO tags(ino, tag) VALUES(?,?)", (ino, t))
    print(f"已打标签 [{t}] → {path}")


def do_untag(conn: sqlite3.Connection, path: str, t: str) -> None:
    ino = get_ino(conn, path)
    if not ino:
        print(f"文件不存在: {path}", file=sys.stderr); return
    with conn:
        conn.execute("DELETE FROM tags WHERE ino=? AND tag=?", (ino, t))
    print(f"已去标签 [{t}] → {path}")


def do_rename(conn: sqlite3.Connection, path: str, new_name: str) -> None:
    ino = get_ino(conn, path)
    if not ino:
        print(f"文件不存在: {path}", file=sys.stderr); return
    parent = os.path.dirname(path)
    new_path = os.path.join(parent, new_name)
    # 先改真实文件，再改元数据（若文件操作失败，元数据不动）
    os.rename(path, new_path)
    with conn:
        conn.execute("UPDATE files SET name=?, path=? WHERE ino=?", (new_name, new_path, ino))
    print(f"已重命名 → {new_path}")


def do_rm(conn: sqlite3.Connection, path: str) -> None:
    ino = get_ino(conn, path)
    if not ino:
        print(f"文件不存在: {path}", file=sys.stderr); return
    # 先删真实文件，再删元数据（级联删 tags）
    if os.path.isdir(path):
        os.rmdir(path)
    else:
        os.remove(path)
    with conn:
        conn.execute("DELETE FROM files WHERE ino=?", (ino,))
    print(f"已删除 → {path}")


def main() -> int:
    if len(sys.argv) < 3:
        print(__doc__)
        return 0
    conn = connect()
    cmd, a, b = sys.argv[1], sys.argv[2], (sys.argv[3] if len(sys.argv) > 3 else "")
    a = os.path.abspath(a)  # 统一转绝对路径（与 scan.py 存储的一致）
    if cmd == "tag":
        do_tag(conn, a, b)
    elif cmd == "untag":
        do_untag(conn, a, b)
    elif cmd == "rename":
        do_rename(conn, a, b)
    elif cmd == "rm":
        do_rm(conn, a)
    else:
        print(f"未知命令: {cmd}", file=sys.stderr)
        return 2
    conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
