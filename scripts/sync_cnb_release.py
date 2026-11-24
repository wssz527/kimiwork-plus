#!/usr/bin/env python3
"""Sync a built KimiWork + release tag and assets to the CNB domestic mirror."""
import argparse
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


DEFAULT_CNB_API = "https://api.cnb.cool"
DEFAULT_CNB_WEB = "https://cnb.cool"
DEFAULT_CNB_REPO = "wssz527/kimiwork-plus"
CNB_ACCEPT = "application/vnd.cnb.api+json"
DEFAULT_LOCAL_TOKEN_PATH = Path.home() / "Library/Application Support/kimi-work-plus/release-signing/cnb-token"


def _is_checksum_file(name: str) -> bool:
    upper = name.upper()
    return upper == "SHA256SUMS" or upper.startswith("SHA256SUMS")


def _is_manifest_file(name: str) -> bool:
    lower = name.lower()
    return lower == "manifest.json" or (lower.startswith("update-manifest") and lower.endswith(".json"))


def ordered_assets(paths):
    """Order release files so checksums upload after binaries and manifests upload last."""
    resolved = [Path(item) for item in paths]
    missing = [str(item) for item in resolved if not item.is_file()]
    if missing:
        raise SystemExit(f'Missing release asset(s): {", ".join(missing)}')
    seen = {}
    for item in resolved:
        if item.name in seen:
            raise SystemExit(f"Duplicate asset filename: {item.name}")
        seen[item.name] = item
    regular = [item for item in resolved if not _is_checksum_file(item.name) and not _is_manifest_file(item.name)]
    sums = [item for item in resolved if _is_checksum_file(item.name)]
    manifests = [item for item in resolved if _is_manifest_file(item.name)]
    return regular + sums + manifests


def is_prerelease(tag: str) -> bool:
    version = tag.removeprefix("v")
    return "-" in version.split("+", 1)[0]


def resolve_token(env_token: str = "", token_file: str = "", default_path: Path = DEFAULT_LOCAL_TOKEN_PATH) -> str:
    token = (env_token or "").strip()
    if token:
        return token
    candidates = []
    if token_file:
        candidates.append(Path(token_file))
    elif default_path is not None:
        candidates.append(default_path)
    for path in candidates:
        if path.is_file():
            value = path.read_text(encoding="utf-8").strip()
            if value:
                return value
    return ""


def cnb_json_request(url, token, method="GET", payload=None, timeout=30):
    headers = {
        "Authorization": f"Bearer {token}",
        "Accept": CNB_ACCEPT,
        "User-Agent": "KimiWorkPlus-ReleaseSync/1.0",
    }
    data = None
    if payload is not None:
        data = json.dumps(payload).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw.decode("utf-8")) if raw else {})
    except urllib.error.HTTPError as err:
        try:
            body = err.read().decode("utf-8", errors="replace")[:500]
        finally:
            err.close()
        return err.code, {"_raw": body}


def upload_file_put(upload_url, file_path, timeout=300):
    size = file_path.stat().st_size
    headers = {
        "Content-Length": str(size),
        "Content-Type": "application/octet-stream",
    }
    with file_path.open("rb") as handle:
        req = urllib.request.Request(upload_url, data=handle, headers=headers, method="PUT")
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                resp.read()
                if resp.status < 200 or resp.status >= 300:
                    raise RuntimeError(f"Upload PUT failed for {file_path.name}: HTTP {resp.status}")
        except urllib.error.HTTPError as err:
            try:
                err.read()
            finally:
                err.close()
            raise RuntimeError(f"Upload PUT failed for {file_path.name}: HTTP {err.code}") from None


def ensure_release(api_base, repo, token, tag, notes, prerelease):
    encoded_tag = urllib.parse.quote(tag, safe="")
    tag_url = f"{api_base}/{repo}/-/releases/tags/{encoded_tag}"
    status, data = cnb_json_request(tag_url, token, method="GET")
    if status == 200 and data.get("id"):
        return str(data["id"])
    if status != 404:
        raise RuntimeError(f'Failed to query CNB release {tag}: HTTP {status} {data.get("_raw", "")}')

    create_url = f"{api_base}/{repo}/-/releases"
    payload = {
        "tag_name": tag,
        "name": tag,
        "body": notes,
        "draft": False,
        "prerelease": bool(prerelease),
        "make_latest": "false" if prerelease else "true",
    }
    status, data = cnb_json_request(create_url, token, method="POST", payload=payload)
    if status not in (200, 201) or not data.get("id"):
        raise RuntimeError(f'Failed to create CNB release {tag}: HTTP {status} {data.get("_raw", "")}')
    return str(data["id"])


def upload_release_asset(api_base, repo, token, release_id, file_path, attempts=3):
    size = file_path.stat().st_size
    last_err = None
    for attempt in range(1, attempts + 1):
        try:
            req_url = f"{api_base}/{repo}/-/releases/{release_id}/asset-upload-url"
            status, data = cnb_json_request(
                req_url,
                token,
                method="POST",
                payload={"asset_name": file_path.name, "size": size, "overwrite": True},
            )
            if status < 200 or status >= 300:
                raise RuntimeError(f'asset-upload-url failed for {file_path.name}: HTTP {status} {data.get("_raw", "")}')
            upload_url = data.get("upload_url") or ""
            verify_url = data.get("verify_url") or ""
            if not upload_url or not verify_url:
                raise RuntimeError(f"Missing upload_url/verify_url for {file_path.name}")
            upload_parsed = urllib.parse.urlparse(upload_url)
            api_parsed = urllib.parse.urlparse(api_base)
            if (
                upload_parsed.scheme not in ("http", "https")
                or not upload_parsed.netloc
                or (api_parsed.scheme == "https" and upload_parsed.scheme != "https")
            ):
                raise RuntimeError(f"Refusing untrusted upload_url for {file_path.name}: {upload_url}")
            verify_url = urllib.parse.urljoin(api_base.rstrip("/") + "/", verify_url)
            verify_parsed = urllib.parse.urlparse(verify_url)
            if (
                verify_parsed.scheme not in ("http", "https")
                or verify_parsed.netloc.lower() != api_parsed.netloc.lower()
                or (api_parsed.scheme == "https" and verify_parsed.scheme != "https")
            ):
                raise RuntimeError(f"Refusing untrusted verify_url for {file_path.name}: {verify_url}")

            upload_file_put(upload_url, file_path)

            verify_status, verify_data = cnb_json_request(verify_url, token, method="POST")
            if verify_status < 200 or verify_status >= 300:
                raise RuntimeError(
                    f'asset confirmation failed for {file_path.name}: HTTP {verify_status} {verify_data.get("_raw", "")}'
                )
            return
        except Exception as err:
            last_err = err
            if attempt < attempts:
                time.sleep(attempt)
    raise RuntimeError(f"Failed to upload {file_path.name} after {attempts} attempts: {last_err}")


def push_git_refs(web_base, repo, token, tag, prerelease):
    remote_url = f'{web_base.rstrip("/")}/{repo}.git'
    with tempfile.NamedTemporaryFile("w", prefix="cnb-askpass-", suffix=".sh", delete=False, encoding="utf-8") as script:
        script.write(
            "#!/bin/sh\n"
            'case "$1" in\n'
            '  *Username*|*username*) printf "%s\\n" "cnb" ;;\n'
            '  *) printf "%s\\n" "$CNB_GIT_PASSWORD" ;;\n'
            "esac\n"
        )
        askpass = Path(script.name)
    try:
        askpass.chmod(stat.S_IRWXU)
        env = os.environ.copy()
        env["GIT_ASKPASS"] = str(askpass)
        env["GIT_TERMINAL_PROMPT"] = "0"
        env["CNB_GIT_PASSWORD"] = token
        if not prerelease:
            result = subprocess.run(
                ["git", "-c", "credential.helper=", "push", remote_url, f"refs/tags/{tag}^{{commit}}:refs/heads/main"],
                env=env,
                check=False,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                timeout=120,
            )
            if result.returncode != 0:
                raise RuntimeError(f"git push main to CNB failed: {result.stderr.strip()}")
        result = subprocess.run(
            ["git", "-c", "credential.helper=", "push", remote_url, f"refs/tags/{tag}:refs/tags/{tag}"],
            env=env,
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=120,
        )
        if result.returncode != 0:
            raise RuntimeError(f"git push tag {tag} to CNB failed: {result.stderr.strip()}")
    finally:
        try:
            askpass.unlink()
        except OSError:
            pass


def sync_release(api_base, web_base, repo, token, tag, notes, files, prerelease, push_git=True):
    api_base = api_base.rstrip("/")
    assets = ordered_assets(files)
    if push_git:
        push_git_refs(web_base, repo, token, tag, prerelease)
    release_id = ensure_release(api_base, repo, token, tag, notes, prerelease)
    for asset_path in assets:
        print(f"Uploading {asset_path.name} ({asset_path.stat().st_size} bytes) to CNB {repo} {tag}...", flush=True)
        upload_release_asset(api_base, repo, token, release_id, asset_path)
    return release_id, [p.name for p in assets]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True, help="Release tag name, e.g. v1.1.5")
    parser.add_argument("--repo", default=os.environ.get("CNB_REPO", DEFAULT_CNB_REPO), help="CNB repo slug")
    parser.add_argument("--api-base", default=os.environ.get("CNB_API_BASE", DEFAULT_CNB_API))
    parser.add_argument("--web-base", default=os.environ.get("CNB_WEB_BASE", DEFAULT_CNB_WEB))
    parser.add_argument("--notes", default=os.environ.get("RELEASE_NOTES", ""))
    parser.add_argument("--notes-file", default="", help="Path to file containing release notes")
    parser.add_argument("--token-file", default="", help="Optional path to file containing CNB token")
    parser.add_argument("--prerelease", action="store_true", default=None)
    parser.add_argument("--skip-git-push", action="store_true", help="Skip pushing git tag/main to CNB")
    parser.add_argument("files", nargs="+", help="Release asset files to upload")
    args = parser.parse_args()

    token = resolve_token(os.environ.get("CNB_TOKEN", ""), args.token_file)
    if not token:
        raise SystemExit("CNB_TOKEN environment variable or --token-file is required")

    notes = args.notes
    if args.notes_file:
        notes = Path(args.notes_file).read_text(encoding="utf-8").strip()

    prerelease = is_prerelease(args.tag) if args.prerelease is None else args.prerelease
    release_id, uploaded = sync_release(
        api_base=args.api_base,
        web_base=args.web_base,
        repo=args.repo,
        token=token,
        tag=args.tag,
        notes=notes,
        files=args.files,
        prerelease=prerelease,
        push_git=not args.skip_git_push,
    )
    print(f'Synced CNB release {args.tag} (id={release_id}): {", ".join(uploaded)}')


if __name__ == "__main__":
    main()
