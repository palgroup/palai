# Base pinned by digest: mutable tags are not release inputs. The tag is kept
# for humans; the digest is what the build resolves.
FROM python:3.14.3-slim@sha256:5e59aae31ff0e87511226be8e2b94d78c58f05216efda3b07dbbed938ec8583b

WORKDIR /app

# The engine is pure standard-library Python: no dependencies to install, so the
# image is just the pinned runtime plus source. No build secrets.
COPY src/ /app/src/

ENV PYTHONPATH=/app/src \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

# Non-root, matching the supervisor's sandbox user (packages/runner).
USER 65532:65532

# Absolute interpreter path: the runner's hardened sandbox runs with an empty PATH
# (packages/runner buildEnv), so a bare "python" would fail to exec. The path is the
# pinned base image's interpreter.
ENTRYPOINT ["/usr/local/bin/python", "-m", "palai_engine"]
