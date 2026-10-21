from app.api import UserService


def test_get():
    svc = UserService(":memory:")
    assert svc.get("") is None
