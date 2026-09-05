#!/usr/bin/env python3

import argparse
import gzip
import hashlib
import json
from pathlib import Path, PurePosixPath
import platform
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.request

PYTHON_VERSION = "3.12.14"
PYTHON_RELEASE = "20260825"
MITMPROXY_VERSION = "12.2.3"
ROOT = Path(__file__).resolve().parent.parent
ASSET_DIRECTORY = ROOT / "internal" / "trafficcapture" / "assets"
LOCK_DIRECTORY = ROOT / "scripts" / "mitmproxy-runtime"
TARGETS = {
    "darwin-arm64": {
        "archive": (
            "cpython-3.12.14+20260825-aarch64-apple-darwin-"
            "install_only_stripped.tar.gz"
        ),
        "sha256": (
            "8b0f1fa71eab7ca644e482c631807a1116fa848491051cd1c8d9429491de63a6"
        ),
        "platform": "macosx_11_0_arm64",
        "library": "lib/libpython3.12.dylib",
    },
    "linux-amd64": {
        "archive": (
            "cpython-3.12.14+20260825-x86_64-unknown-linux-gnu-"
            "install_only_stripped.tar.gz"
        ),
        "sha256": (
            "7ce4a71285d913955a76053cc7605ea96da8ecada54dba9cf395245961816421"
        ),
        "platform": "manylinux2014_x86_64",
        "library": "lib/libpython3.12.so.1.0",
    },
    "linux-arm64": {
        "archive": (
            "cpython-3.12.14+20260825-aarch64-unknown-linux-gnu-"
            "install_only_stripped.tar.gz"
        ),
        "sha256": (
            "4c250ec7cea2aedde2b2e8925d7aaf5ba4924895469d6b5c81c7bdc453341c65"
        ),
        "platform": "manylinux2014_aarch64",
        "library": "lib/libpython3.12.so.1.0",
    },
}


def digest(path: Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def download(url: str, destination: Path, expected: str) -> None:
    request = urllib.request.Request(
        url,
        headers={"User-Agent": "Crux-Asset-Builder"},
    )
    with (
        urllib.request.urlopen(request, timeout=300) as response,
        destination.open("wb") as output,
    ):
        shutil.copyfileobj(response, output)
    actual = digest(destination)
    if actual != expected:
        raise RuntimeError(
            f"checksum mismatch for {destination.name}: "
            f"expected {expected}, got {actual}"
        )


def link_stays_within_python(
    member_path: PurePosixPath,
    member: tarfile.TarInfo,
) -> bool:
    target = PurePosixPath(member.linkname)
    if target.is_absolute():
        return False
    if member.issym():
        target = member_path.parent / target
    parts = []
    for part in target.parts:
        if part in {"", "."}:
            continue
        if part == "..":
            if not parts:
                return False
            parts.pop()
        else:
            parts.append(part)
    return bool(parts) and parts[0] == "python"


def extract_python(archive: Path, destination: Path) -> Path:
    with tarfile.open(archive, "r:gz") as source:
        for member in source.getmembers():
            path = PurePosixPath(member.name)
            if (
                not path.parts
                or path.parts[0] != "python"
                or path.is_absolute()
                or ".." in path.parts
            ):
                raise RuntimeError(
                    f"unsafe Python archive member: {member.name}"
                )
            if not (
                member.isdir()
                or member.isfile()
                or member.issym()
                or member.islnk()
            ):
                raise RuntimeError(
                    f"unsupported Python archive member: {member.name}"
                )
            if (
                (member.issym() or member.islnk())
                and not link_stays_within_python(path, member)
            ):
                raise RuntimeError(
                    f"unsafe Python archive link: {member.name}"
                )
        if sys.version_info >= (3, 12):
            source.extractall(destination, filter="data")
        else:
            source.extractall(destination)
    return destination / "python"


def remove_path(path: Path) -> None:
    if path.is_symlink() or path.is_file():
        path.unlink(missing_ok=True)
    elif path.is_dir():
        shutil.rmtree(path)


def install_packages(target: str, python_root: Path) -> None:
    site_packages = (
        python_root / "lib" / "python3.12" / "site-packages"
    )
    patterns = (
        "pip",
        "pip-*",
        "setuptools",
        "setuptools-*",
        "_distutils_hack",
    )
    for pattern in patterns:
        for path in site_packages.glob(pattern):
            remove_path(path)
    command = [
        sys.executable,
        "-m",
        "pip",
        "install",
        "--disable-pip-version-check",
        "--no-compile",
        "--no-deps",
        "--require-hashes",
        "--only-binary=:all:",
        "--platform",
        TARGETS[target]["platform"],
        "--python-version",
        "3.12",
        "--implementation",
        "cp",
        "--abi",
        "cp312",
        "--target",
        str(site_packages),
        "--requirement",
        str(LOCK_DIRECTORY / f"requirements-{target}.txt"),
    ]
    subprocess.run(command, check=True)


def prune_runtime(python_root: Path) -> None:
    python_library = python_root / "lib" / "python3.12"
    site_packages = python_library / "site-packages"
    for path in (
        python_root / "bin",
        python_root / "include",
        site_packages / "bin",
    ):
        remove_path(path)
    for path in (python_root / "lib").glob("libpython*.a"):
        remove_path(path)
    for path in python_library.glob("config-*"):
        remove_path(path)
    for path in python_root.rglob("*.app"):
        remove_path(path)
    for path in python_root.rglob("__pycache__"):
        remove_path(path)
    for path in python_root.rglob("*.pyc"):
        remove_path(path)


def runtime_manifest(target: str) -> dict[str, str]:
    lock = LOCK_DIRECTORY / f"requirements-{target}.txt"
    return {
        "target": target,
        "python": PYTHON_VERSION,
        "python_build": PYTHON_RELEASE,
        "mitmproxy": MITMPROXY_VERSION,
        "layout": "in-process-v1",
        "requirements_sha256": digest(lock),
        "library": TARGETS[target]["library"],
    }


def write_manifest(target: str, python_root: Path) -> None:
    value = runtime_manifest(target)
    destination = python_root / "CRUX_RUNTIME.json"
    destination.write_text(json.dumps(value, sort_keys=True) + "\n")


def validate_runtime(target: str, python_root: Path) -> None:
    library = python_root / TARGETS[target]["library"]
    if not library.is_file():
        raise RuntimeError(f"runtime is missing {library}")
    required = (
        python_root
        / "lib"
        / "python3.12"
        / "site-packages"
        / "mitmproxy"
        / "tools"
        / "web"
        / "master.py"
    )
    if not required.is_file():
        raise RuntimeError("runtime is missing mitmproxy WebMaster")
    forbidden = []
    names = {"python", "python3", "python3.12", "mitmdump", "mitmweb"}
    for path in python_root.rglob("*"):
        relative = path.relative_to(python_root)
        lowered = str(relative).lower()
        if "bin" in relative.parts:
            forbidden.append(str(relative))
        if lowered.endswith(".app"):
            forbidden.append(str(relative))
        if (path.is_file() or path.is_symlink()) and path.name in names:
            forbidden.append(str(relative))
    if forbidden:
        values = ", ".join(forbidden[:20])
        raise RuntimeError(
            "runtime contains forbidden executables: " + values
        )


def add_archive_entry(
    output: tarfile.TarFile,
    root: Path,
    path: Path,
) -> None:
    relative = path.relative_to(root)
    info = output.gettarinfo(str(path), arcname=relative.as_posix())
    info.uid = 0
    info.gid = 0
    info.uname = ""
    info.gname = ""
    info.mtime = 0
    if info.isfile():
        with path.open("rb") as source:
            output.addfile(info, source)
    else:
        output.addfile(info)


def create_archive(target: str, python_root: Path) -> Path:
    ASSET_DIRECTORY.mkdir(parents=True, exist_ok=True)
    destination = (
        ASSET_DIRECTORY / f"mitmproxy-runtime-{target}.tar.gz"
    )
    temporary = destination.with_suffix(destination.suffix + ".tmp")
    with (
        temporary.open("wb") as raw,
        gzip.GzipFile(
            fileobj=raw,
            mode="wb",
            compresslevel=9,
            mtime=0,
        ) as compressed,
        tarfile.open(fileobj=compressed, mode="w") as output,
    ):
        paths = sorted(
            python_root.rglob("*"),
            key=lambda value: value.relative_to(
                python_root
            ).as_posix(),
        )
        for path in paths:
            add_archive_entry(output, python_root, path)
    temporary.replace(destination)
    return destination


def existing_archive_is_current(target: str, path: Path) -> bool:
    if not path.is_file():
        return False
    try:
        with tarfile.open(path, "r:gz") as archive:
            member = archive.getmember("CRUX_RUNTIME.json")
            source = archive.extractfile(member)
            if source is None:
                return False
            value = json.load(source)
    except (
        OSError,
        KeyError,
        tarfile.TarError,
        json.JSONDecodeError,
    ):
        return False
    return value == runtime_manifest(target)


def build_target(target: str, force: bool) -> None:
    definition = TARGETS[target]
    output = ASSET_DIRECTORY / f"mitmproxy-runtime-{target}.tar.gz"
    if not force and existing_archive_is_current(target, output):
        print(f"{target}: using existing {output}")
        return
    prefix = f"crux-mitmproxy-{target}-"
    with tempfile.TemporaryDirectory(prefix=prefix) as raw:
        temporary = Path(raw)
        archive = temporary / definition["archive"]
        url_name = definition["archive"].replace("+", "%2B")
        url = (
            "https://github.com/astral-sh/python-build-standalone/"
            f"releases/download/{PYTHON_RELEASE}/{url_name}"
        )
        download(url, archive, definition["sha256"])
        python_root = extract_python(archive, temporary / "source")
        install_packages(target, python_root)
        prune_runtime(python_root)
        write_manifest(target, python_root)
        validate_runtime(target, python_root)
        output = create_archive(target, python_root)
        print(
            f"{target}: {output} "
            f"({output.stat().st_size} bytes, sha256:{digest(output)})"
        )


def current_target() -> str:
    system = platform.system().lower()
    machine = platform.machine().lower()
    if system == "darwin" and machine in {"arm64", "aarch64"}:
        return "darwin-arm64"
    if system == "linux" and machine in {"x86_64", "amd64"}:
        return "linux-amd64"
    if system == "linux" and machine in {"arm64", "aarch64"}:
        return "linux-arm64"
    raise RuntimeError(f"unsupported build platform: {system}/{machine}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--target",
        choices=["current", "all", *TARGETS],
        default="current",
    )
    parser.add_argument("--force", action="store_true")
    args = parser.parse_args()
    targets = (
        list(TARGETS)
        if args.target == "all"
        else [
            current_target()
            if args.target == "current"
            else args.target
        ]
    )
    for target in targets:
        build_target(target, args.force)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
