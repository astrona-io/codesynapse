import subprocess
from flask import Flask, request

from .db import connect, find_user

app = Flask(__name__)


class UserService:
    """Serves users."""

    def __init__(self, path):
        self.conn = connect(path)

    def get(self, name):
        """Fetch one user."""
        if not name:
            return None
        return self._load(name)

    def _load(self, name):
        def inner():
            return "def not_a_function():"
        return find_user(self.conn, name)


@app.route("/run")
def run_job():
    cmd = request.args.get("cmd")
    subprocess.run(f"job {cmd}", shell=True)
    return "ok"


def unused_helper():
    return eval("1+1")
