import os
import re
import sys


def parse_mysql_dsn(dsn: str):
    m = re.match(r"^(?P<user>[^:]+)(:(?P<pw>[^@]*))?@tcp\((?P<host>[^)]+)\)/(?P<db>[^?]+)", dsn)
    if not m:
        raise ValueError("unsupported MYSQL_DSN format")
    user = m.group("user")
    pw = m.group("pw") or ""
    hostport = m.group("host")
    dbname = m.group("db")
    if ":" in hostport:
        host, port_s = hostport.rsplit(":", 1)
        port = int(port_s)
    else:
        host, port = hostport, 3306
    return user, pw, host, port, dbname


def main():
    dsn = os.environ.get("MYSQL_DSN", "").strip()
    if not dsn:
        print("MYSQL_DSN is required")
        return 1

    try:
        user, pw, host, port, dbname = parse_mysql_dsn(dsn)
    except Exception as e:
        print("parse MYSQL_DSN failed:", str(e))
        return 1

    try:
        import mysql.connector  # type: ignore
    except Exception:
        print("missing dependency: mysql-connector-python")
        print("install: python -m pip install mysql-connector-python")
        return 2

    conn = mysql.connector.connect(user=user, password=pw, host=host, port=port, database=dbname)
    cur = conn.cursor()

    cur.execute("SHOW DATABASES")
    dbs = [r[0] for r in cur.fetchall()]
    print("databases:")
    for d in dbs:
        print("  -", d)

    cur.execute("SELECT TABLE_NAME FROM information_schema.tables WHERE table_schema = DATABASE() ORDER BY table_name")
    tables = [r[0] for r in cur.fetchall()]
    print("\ncurrent_database:", dbname)
    print("tables:")
    for t in tables:
        print("  -", t)

    suspect = [t for t in tables if re.search(r"(api|center|channel|provider|key|credential)", t, re.IGNORECASE)]
    if suspect:
        print("\nsuspected_apicenter_tables:")
        for t in suspect:
            print("  -", t)
            cur.execute(
                "SELECT COLUMN_NAME, DATA_TYPE FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = %s ORDER BY ordinal_position",
                (t,),
            )
            cols = cur.fetchall()
            for name, typ in cols:
                print("      ", f"{name} ({typ})")

    cur.close()
    conn.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

