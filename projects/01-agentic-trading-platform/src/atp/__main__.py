"""`python -m atp` starts the API server."""

import os

import uvicorn


def main() -> None:
    uvicorn.run(
        "atp.api:create_app",
        factory=True,
        host=os.environ.get("ATP_HOST", "127.0.0.1"),
        port=int(os.environ.get("ATP_PORT", "8000")),
    )


if __name__ == "__main__":
    main()
