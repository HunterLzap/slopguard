from typing import Any

value: Any = None  # type: ignore
print(value)  # noqa


def greet(name: str) -> str:
    return f"hello {name}"
