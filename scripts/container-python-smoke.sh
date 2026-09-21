#!/usr/bin/env bash
set -euo pipefail
image_name="${1:?usage: bash scripts/container-python-smoke.sh IMAGE}"
# Named volume is seeded from the image's owned directory, as in Compose.
volume_name="runtime-python-smoke-${$}"
docker volume create "$volume_name" >/dev/null
trap 'docker volume rm "$volume_name" >/dev/null' EXIT
docker run --rm --read-only --cap-drop ALL --security-opt no-new-privileges \
 --ulimit core=0 --tmpfs /tmp:rw,nosuid,nodev,size=64m \
 --mount "type=volume,source=$volume_name,target=/tmp/agent-runtime-workspaces" \
 --entrypoint /bin/sh "$image_name" -eu -c '
  test "$(id -u)" != 0
  test ! -e /etc/agent-runtime/apps.json
  root=/tmp/agent-runtime-workspaces/smoke
  state=/tmp/agent-runtime-ephemeral/smoke
  export HOME="$state/home" TMPDIR="$state/tmp" PIP_CACHE_DIR="$state/cache" PIP_REQUIRE_VIRTUALENV=true
  mkdir -p "$HOME" "$TMPDIR" "$PIP_CACHE_DIR"
  python3 -I -m venv --without-pip "$state/venv"
  test ! -e "$state/venv/bin/pip"
  python3 -m pip --python "$state/venv/bin/python3" --version
  "$state/venv/bin/python3" -c "import os,pathlib,zipfile; state=pathlib.Path(os.environ[\"HOME\"]).parent; z=zipfile.ZipFile(state/\"smoke_package-1.0-py3-none-any.whl\",\"w\"); data={\"smoke_package/__init__.py\":\"value = 42\n\",\"smoke_package-1.0.dist-info/METADATA\":\"Metadata-Version: 2.1\nName: smoke-package\nVersion: 1.0\n\",\"smoke_package-1.0.dist-info/WHEEL\":\"Wheel-Version: 1.0\nRoot-Is-Purelib: true\nTag: py3-none-any\n\",\"smoke_package-1.0.dist-info/RECORD\":\"\"}; [z.writestr(n,c) for n,c in data.items()]; z.close()"
  PIP_NO_CACHE_DIR=true PIP_NO_COMPILE=true python3 -m pip --python "$state/venv/bin/python3" install --no-index --no-deps "$state/smoke_package-1.0-py3-none-any.whl"
  "$state/venv/bin/python3" -c "import smoke_package; assert smoke_package.value == 42"
  "$state/venv/bin/python3" -c "import pathlib,tempfile; p=pathlib.Path(\"$root/result.csv\"); p.write_text(\"value\\n42\\n\"); assert pathlib.Path(tempfile.gettempdir()).is_relative_to(\"/tmp/agent-runtime-ephemeral\")"
  "$state/venv/bin/python3" -c "import pathlib; assert \"42\" in pathlib.Path(\"$root/result.csv\").read_text()"
  test -w "$PIP_CACHE_DIR"
  echo "Ephemeral venv and caches plus durable workspace outputs work with a read-only root"
 '
