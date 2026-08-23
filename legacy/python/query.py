#!/usr/bin/env python3
# Historical prototype only. The supported implementation is the Go CLI.
"""agent-fs 查询 CLI：让 agent 用 SQL 语义操作文件系统。

用法：
    python3 query.py "SELECT ... FROM files WHERE ..."   # 任意 SQL
    python3 query.py --ls /path                          # 列目录
    python3 query.py --find 关键词                       # 按内容/文件名搜索
    python3 query.py --big 10                            # 找 >10MB 的文件
    python3 query.py --du /path                          # 目录占空间
    python3 query.py --tag 项目                          # 按标签找文件
    python3 query.py --ext py                            # 按扩展名找文件

核心：agent 不读字节，而是用 SQL 查文件的「数据语义」——过滤/聚合/排序/
事务，全都交给 SQLite 的查询引擎。
"""
import os
import sqlite3
import sys


def db_path() -> str:
    return os.path.join(os.path.dirname(os.path.abspath(__file__)), "fs.db")


def connect() -> sqlite3.Connection:
    conn = sqlite3.connect(db_path())
    conn.row_factory = sqlite3.Row  # 按列名访问
    return conn


def print_rows(cur: sqlite3.Cursor) -> None:
    """把查询结果按对齐表格打印。"""
    cols = [d[0] for d in cur.description]
    rows = cur.fetchall()
    if not rows:
        print("(无结果)")
        return
    # 计算每列宽度
    widths = [len(c) for c in cols]
    for r in rows:
        for i, c in enumerate(cols):
            widths[i] = max(widths[i], len(str(r[c])))
    # 表头
    header = "  ".join(c.ljust(widths[i]) for i, c in enumerate(cols))
    print(header)
    print("  ".join("-" * widths[i] for i in range(len(cols))))
    for r in rows:
        print("  ".join(str(r[c]).ljust(widths[i]) for i, c in enumerate(cols)))
    print(f"\n({len(rows)} 行)")


def run_sql(conn: sqlite3.Connection, sql: str) -> None:
    print_rows(conn.execute(sql))


def ls(conn: sqlite3.Connection, path: str) -> None:
    """列目录：查 parent_ino = 该目录 ino 的行。"""
    sql = """
        SELECT name, kind, size, datetime(mtime,'unixepoch') AS mtime
        FROM files
        WHERE parent_ino = (SELECT ino FROM files WHERE path = ?)
        ORDER BY kind, name
    """
    print_rows(conn.execute(sql, (path,)))


def find(conn: sqlite3.Connection, keyword: str) -> None:
    """全文搜索：文件名或内容预览含关键词。"""
    sql = """
        SELECT path, kind, size
        FROM files
        WHERE name LIKE ? OR content_head LIKE ?
        ORDER BY size DESC
        LIMIT 50
    """
    like = f"%{keyword}%"
    print_rows(conn.execute(sql, (like, like)))


def big(conn: sqlite3.Connection, mb: int) -> None:
    """找大文件（> mb MB）。"""
    sql = """
        SELECT path, size FROM files
        WHERE kind = 'file' AND size > ?
        ORDER BY size DESC LIMIT 50
    """
    print_rows(conn.execute(sql, (mb * 1024 * 1024,)))


def du(conn: sqlite3.Connection, path: str) -> None:
    """目录占空间：递归聚合子树大小。"""
    sql = """
        WITH RECURSIVE sub(ino) AS (
            SELECT ino FROM files WHERE path = ?
            UNION ALL
            SELECT f.ino FROM files f JOIN sub s ON f.parent_ino = s.ino
        )
        SELECT COUNT(*) AS files, SUM(size) AS total_bytes
        FROM files WHERE ino IN (SELECT ino FROM sub)
    """
    print_rows(conn.execute(sql, (path,)))


def tag(conn: sqlite3.Connection, t: str) -> None:
    """按标签找文件。"""
    sql = """
        SELECT f.path, f.kind, f.size
        FROM files f JOIN tags t ON f.ino = t.ino
        WHERE t.tag = ?
        ORDER BY f.size DESC
    """
    print_rows(conn.execute(sql, (t,)))


def ext(conn: sqlite3.Connection, e: str) -> None:
    """按扩展名找文件。"""
    sql = """
        SELECT path, size FROM files
        WHERE kind = 'file' AND name LIKE ?
        ORDER BY size DESC LIMIT 50
    """
    print_rows(conn.execute(sql, (f"%.{e}",)))


def main() -> int:
    if len(sys.argv) < 2:
        print(__doc__)
        return 0
    conn = connect()
    arg = sys.argv[1]
    if arg == "--ls" and len(sys.argv) > 2:
        ls(conn, sys.argv[2])
    elif arg == "--find" and len(sys.argv) > 2:
        find(conn, sys.argv[2])
    elif arg == "--big" and len(sys.argv) > 2:
        big(conn, int(sys.argv[2]))
    elif arg == "--du" and len(sys.argv) > 2:
        du(conn, sys.argv[2])
    elif arg == "--tag" and len(sys.argv) > 2:
        tag(conn, sys.argv[2])
    elif arg == "--ext" and len(sys.argv) > 2:
        ext(conn, sys.argv[2])
    elif arg.startswith("--"):
        print(f"未知命令: {arg}", file=sys.stderr)
        return 2
    else:
        # 任意 SQL
        run_sql(conn, arg)
    conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
