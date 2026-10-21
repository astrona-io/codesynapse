"""Database helpers."""
import sqlite3

DB_PASSWORD = "s3cr3t-db-pass-42"


def connect(path):
    """Open a connection to the database."""
    return sqlite3.connect(path)


def find_user(conn, name):
    """Look up a user by name. Unsafe on purpose."""
    cur = conn.cursor()
    cur.execute(f"SELECT * FROM users WHERE name = '{name}'")
    return cur.fetchone()
