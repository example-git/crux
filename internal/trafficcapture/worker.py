from __future__ import annotations

import asyncio
import contextlib
import json
import os
from pathlib import Path
import platform
import secrets
import subprocess
import tempfile
import time
import urllib.parse

from argon2 import PasswordHasher
import certifi
from mitmproxy import options
from mitmproxy.tools.web.master import WebMaster

PROXY_VARIABLES = (
    "HTTP_PROXY",
    "HTTPS_PROXY",
    "ALL_PROXY",
    "http_proxy",
    "https_proxy",
    "all_proxy",
)
TRUST_BUNDLE_VARIABLES = (
    "SSL_CERT_FILE",
    "REQUESTS_CA_BUNDLE",
    "CURL_CA_BUNDLE",
    "AWS_CA_BUNDLE",
    "GIT_SSL_CAINFO",
    "NIX_SSL_CERT_FILE",
)


def write_json(path: Path, value: dict[str, object]) -> None:
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, ensure_ascii=True))
    temporary.chmod(0o600)
    temporary.replace(path)


def terminate(process: subprocess.Popen[bytes] | None) -> None:
    if process is None or process.poll() is not None:
        return
    process.terminate()
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)


def combined_bundle(certificate: Path, destination: Path) -> Path:
    chunks = [Path(certifi.where()).read_bytes().rstrip() + b"\n"]
    chunks.append(certificate.read_bytes().rstrip() + b"\n")
    destination.write_bytes(b"".join(chunks))
    destination.chmod(0o600)
    return destination


def target_environment(
    base: dict[str, str],
    proxy_url: str,
    certificate: Path,
    trust_bundle: Path,
    unset_names: list[str],
) -> dict[str, str]:
    environment = dict(base)
    for name in unset_names or []:
        environment.pop(name, None)
    for name in PROXY_VARIABLES:
        environment[name] = proxy_url
    for name in TRUST_BUNDLE_VARIABLES:
        environment[name] = str(trust_bundle)
    environment["NODE_EXTRA_CA_CERTS"] = str(certificate)
    environment["DENO_CERT"] = str(trust_bundle)
    if platform.system() == "Darwin":
        setting = "x509sslcertoverrideplatform"
        entries = [
            entry
            for entry in environment.get("GODEBUG", "").split(",")
            if entry and not entry.startswith(setting + "=")
        ]
        entries.append(setting + "=1")
        environment["GODEBUG"] = ",".join(entries)
    return environment


async def endpoint_ready(host: str, port: int, request: bytes) -> bool:
    try:
        reader, writer = await asyncio.wait_for(
            asyncio.open_connection(host, port),
            timeout=0.5,
        )
        writer.write(request)
        await writer.drain()
        response = await asyncio.wait_for(reader.read(16), timeout=0.5)
        writer.close()
        await writer.wait_closed()
        return bool(response)
    except (OSError, TimeoutError, asyncio.TimeoutError):
        return False


async def wait_for_mitmweb(
    task: asyncio.Task[None],
    host: str,
    proxy_port: int,
    viewer_port: int,
    certificate: Path,
) -> None:
    deadline = time.monotonic() + 20
    proxy_request = b"GET http://mitm.it/ HTTP/1.0\r\nHost: mitm.it\r\n\r\n"
    viewer_request = b"GET / HTTP/1.0\r\nHost: localhost\r\n\r\n"
    while time.monotonic() < deadline:
        if task.done():
            await task
            raise RuntimeError("embedded mitmweb stopped during startup")
        if certificate.is_file():
            proxy_ready = await endpoint_ready(host, proxy_port, proxy_request)
            viewer_ready = await endpoint_ready(
                host,
                viewer_port,
                viewer_request,
            )
            if proxy_ready and viewer_ready:
                return
        await asyncio.sleep(0.1)
    raise RuntimeError(
        "embedded mitmweb did not complete proxy and viewer startup "
        "within 20 seconds"
    )


async def wait_for_parent_ready(path: Path) -> None:
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        if path.exists():
            path.unlink(missing_ok=True)
            return
        await asyncio.sleep(0.05)
    raise RuntimeError(
        "tmux output capture was not activated within 20 seconds"
    )


async def run_worker() -> int:
    config_path = Path(os.environ.pop("CRUX_TRAFFIC_CAPTURE_CONFIG"))
    try:
        config = json.loads(config_path.read_text())
    finally:
        config_path.unlink(missing_ok=True)
    status_path = Path(config["status_path"])
    runtime_path = Path(config["runtime_path"])
    ready_path = Path(config["ready_path"])
    stop_path = Path(config["stop_path"])
    output = Path(config["output"])
    status: dict[str, object] = {
        "state": "waiting",
        "session": config["session"],
        "tmux_socket": "crux-capture",
        "capture": str(output),
        "pane_log": config["pane_log"],
    }
    write_json(status_path, status)
    target: subprocess.Popen[bytes] | None = None
    master: WebMaster | None = None
    master_task: asyncio.Task[None] | None = None
    return_code = 1
    try:
        await wait_for_parent_ready(ready_path)
        status["state"] = "starting"
        write_json(status_path, status)
        with tempfile.TemporaryDirectory(
            prefix="ca-",
            dir=runtime_path,
        ) as temporary:
            ca_path = Path(temporary)
            certificate = ca_path / "mitmproxy-ca-cert.pem"
            token = secrets.token_urlsafe(32)
            opts = options.Options()
            master = WebMaster(opts, with_termlog=True)
            opts.update(
                confdir=str(ca_path),
                listen_host=config["host"],
                listen_port=int(config["port"]),
                save_stream_file=str(output),
                termlog_verbosity="info",
                web_host=config["host"],
                web_open_browser=False,
                web_password=PasswordHasher().hash(token),
                web_port=int(config["viewer_port"]),
            )
            master_task = asyncio.create_task(master.run())
            await wait_for_mitmweb(
                master_task,
                config["host"],
                int(config["port"]),
                int(config["viewer_port"]),
                certificate,
            )
            trust_bundle = combined_bundle(
                certificate,
                ca_path / "combined-ca.pem",
            )
            proxy_url = f"http://{config['host']}:{config['port']}"
            viewer_url = (
                f"http://{config['host']}:{config['viewer_port']}/?token="
                f"{urllib.parse.quote(token, safe='')}"
            )
            environment = target_environment(
                config["environment"],
                proxy_url,
                certificate,
                trust_bundle,
                config["unset_env"],
            )
            target = subprocess.Popen(
                config["command"],
                cwd=config["cwd"],
                env=environment,
            )
            status.update(
                {
                    "state": "running",
                    "proxy_pid": os.getpid(),
                    "target_pid": target.pid,
                    "proxy": proxy_url,
                    "viewer_url": viewer_url,
                }
            )
            write_json(status_path, status)
            print(f"Capture proxy: {proxy_url}")
            while target.poll() is None:
                if stop_path.exists():
                    terminate(target)
                    return_code = 130
                    break
                if master_task.done():
                    await master_task
                    raise RuntimeError(
                        "embedded mitmweb exited during capture"
                    )
                await asyncio.sleep(0.1)
            else:
                return_code = int(target.returncode or 0)
    except BaseException as error:
        return_code = 130 if isinstance(error, KeyboardInterrupt) else 1
        status.pop("viewer_url", None)
        status.update(
            {
                "state": "failed",
                "error": str(error),
                "exit_code": return_code,
            }
        )
        write_json(status_path, status)
        raise
    finally:
        terminate(target)
        if master is not None:
            master.shutdown()
        if master_task is not None:
            try:
                await asyncio.wait_for(master_task, timeout=10)
            except (TimeoutError, asyncio.TimeoutError):
                master_task.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await master_task
        stop_path.unlink(missing_ok=True)
    if return_code < 0:
        return_code = 128 + abs(return_code)
    status.pop("viewer_url", None)
    status.update(
        {
            "state": "completed" if return_code == 0 else "failed",
            "exit_code": return_code,
        }
    )
    write_json(status_path, status)
    print(f"Capture saved to {output}")
    return return_code


raise SystemExit(asyncio.run(run_worker()))
