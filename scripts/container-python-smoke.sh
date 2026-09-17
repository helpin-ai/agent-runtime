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
  export HOME="$root/home" TMPDIR="$root/tmp" PIP_CACHE_DIR="$root/cache" PIP_REQUIRE_VIRTUALENV=true
  mkdir -p "$HOME" "$TMPDIR" "$PIP_CACHE_DIR"
  python3 -I -m venv "$root/venv"
  "$root/venv/bin/python3" -m pip --version
  "$root/venv/bin/python3" -c "import os,pathlib,zipfile; root=pathlib.Path(os.environ[\"HOME\"]).parent; z=zipfile.ZipFile(root/\"smoke_package-1.0-py3-none-any.whl\",\"w\"); data={\"smoke_package/__init__.py\":\"value = 42\n\",\"smoke_package-1.0.dist-info/METADATA\":\"Metadata-Version: 2.1\nName: smoke-package\nVersion: 1.0\n\",\"smoke_package-1.0.dist-info/WHEEL\":\"Wheel-Version: 1.0\nRoot-Is-Purelib: true\nTag: py3-none-any\n\",\"smoke_package-1.0.dist-info/RECORD\":\"\"}; [z.writestr(n,c) for n,c in data.items()]; z.close()"
  "$root/venv/bin/python3" -m pip install --no-index --no-deps "$root/smoke_package-1.0-py3-none-any.whl"
  "$root/venv/bin/python3" -c "import smoke_package; assert smoke_package.value == 42"
  "$root/venv/bin/python3" -c "import os,pathlib,tempfile; p=pathlib.Path(os.environ[\"HOME\"])/\"result.csv\"; p.write_text(\"value\\n42\\n\"); assert pathlib.Path(tempfile.gettempdir()).is_relative_to(\"/tmp/agent-runtime-workspaces\")"
  "$root/venv/bin/python3" -c "import os,pathlib; assert \"42\" in (pathlib.Path(os.environ[\"HOME\"])/\"result.csv\").read_text()"
  test -w "$PIP_CACHE_DIR"
  echo "Private venv, pip, cache, scratch and retained files work with a read-only root"
 '
