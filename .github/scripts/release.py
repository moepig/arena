#!/usr/bin/env python3
import argparse
import base64
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys
import tempfile
import urllib.error
import urllib.parse
import urllib.request
import zipfile

SEMVER = re.compile(r'^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$')
DIGEST = re.compile(r'^sha256:[0-9a-f]{64}$')
SHA = re.compile(r'^[0-9a-f]{40}$')
RECORD = 'release-manifest.json'
WORKFLOW = '.github/workflows/release-please.yml'


def require(condition, message):
    if not condition:
        raise ValueError(message)


def version(tag):
    match = SEMVER.fullmatch(tag)
    require(match is not None, 'Invalid SemVer tag')
    pre = match[4]
    if pre:
        require(all(not (p.isdigit() and len(p) > 1 and p[0] == '0') for p in pre.split('.')), 'Invalid numeric prerelease')
    return tuple(int(match[i]) for i in (1, 2, 3)), pre


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def artifact_digest(value):
    digest = 'sha256:' + value if re.fullmatch('[0-9a-f]{64}', value) else value
    require(DIGEST.fullmatch(digest), 'Invalid artifact digest')
    return digest


class Redirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        redirected = super().redirect_request(req, fp, code, msg, headers, newurl)
        if urllib.parse.urlparse(req.full_url).netloc != urllib.parse.urlparse(newurl).netloc:
            redirected.remove_header('Authorization')
        return redirected


class GitHub:
    def __init__(self, repository, token):
        require(re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository), 'Invalid repository')
        self.repository = repository
        self.token = token
        self.opener = urllib.request.build_opener(Redirect())

    def request(self, path, method='GET', data=None, binary=False, missing=False, upload=False):
        root = 'https://uploads.github.com/' if upload else 'https://api.github.com/'
        body = data if isinstance(data, bytes) else json.dumps(data).encode() if data is not None else None
        req = urllib.request.Request(root + path.lstrip('/'), data=body, method=method, headers={
            'Authorization': 'Bearer ' + self.token,
            'Accept': 'application/octet-stream' if binary and method == 'GET' and '/releases/assets/' in path else 'application/vnd.github+json',
            'Content-Type': 'application/octet-stream' if isinstance(data, bytes) else 'application/json',
            'X-GitHub-Api-Version': '2022-11-28',
            'User-Agent': 'repository-release',
        })
        try:
            with self.opener.open(req, timeout=120) as response:
                result = response.read()
        except urllib.error.HTTPError as error:
            if missing and error.code == 404:
                return None
            raise ValueError(f'GitHub {method} {path}: HTTP {error.code}') from error
        if binary:
            return result
        return json.loads(result) if result else None

    def repo(self, path, **kwargs):
        return self.request(f'repos/{self.repository}/{path}', **kwargs)

    def pages(self, path):
        page = 1
        while True:
            result = self.repo(path + ('&' if '?' in path else '?') + f'per_page=100&page={page}')
            require(isinstance(result, list), 'Expected paginated array')
            yield from result
            if len(result) < 100:
                break
            page += 1

    def asset(self, asset):
        return self.repo(f"releases/assets/{int(asset['id'])}", binary=True)


def output(key, value):
    text = json.dumps(value, separators=(',', ':')) if isinstance(value, (list, dict)) else str(value).lower() if isinstance(value, bool) else str(value)
    require('\n' not in text and '\r' not in text, 'Multiline job output')
    with open(os.environ['GITHUB_OUTPUT'], 'a') as handle:
        handle.write(f'{key}={text}\n')


def assets_for(policy, tag):
    version(tag)
    return sorted(name.replace('{version}', tag[1:]) for name in policy['assets'])


def containers_for(policy, tag):
    return [dict(item, destination=item['image'] + ':' + tag[1:]) for item in policy.get('containers', [])]


def tag_sha(api, tag):
    ref = api.repo('git/ref/tags/' + urllib.parse.quote(tag, safe=''))['object']
    for _ in range(8):
        if ref['type'] == 'commit':
            require(SHA.fullmatch(ref['sha']), 'Invalid tag SHA')
            return ref['sha']
        require(ref['type'] == 'tag', 'Tag does not resolve to a commit')
        ref = api.repo('git/tags/' + ref['sha'])['object']
    raise ValueError('Tag chain too deep')


def validate_target(api, release_id, identity):
    require(str(release_id).isdigit() and int(release_id) > 0, 'Invalid release ID')
    release = api.repo(f'releases/{int(release_id)}')
    require(release['id'] == int(release_id), 'Release ID mismatch')
    tag = release['tag_name']
    _, pre = version(tag)
    require(release['prerelease'] == bool(pre), 'Prerelease classification mismatch')
    sha = tag_sha(api, tag)
    comparison = api.repo(f'compare/{sha}...main')
    require(comparison['status'] in ('ahead', 'identical') and comparison['merge_base_commit']['sha'] == sha, 'Release is outside main history')
    prs = list(api.pages(f'commits/{sha}/pulls'))
    matches = [pr for pr in prs if pr.get('merged_at') and pr.get('merge_commit_sha') == sha and pr['base']['ref'] == 'main' and pr['base']['repo']['full_name'] == api.repository]
    require(len(matches) == 1, 'Expected one merged release PR')
    pr = api.repo(f"pulls/{matches[0]['number']}")
    require(pr['head']['repo'] and pr['head']['repo']['full_name'] == api.repository, 'Release PR is from another repository')
    require(pr['user']['type'] == 'Bot' and str(pr['user']['id']) == identity['bot_id'] and pr['user']['login'] == identity['bot_login'], 'Unexpected release PR author')
    require(pr['head']['ref'] == 'release-please--branches--main', 'Unexpected release branch')
    reviews = {}
    for review in api.pages(f"pulls/{pr['number']}/reviews"):
        if review.get('submitted_at') and review['submitted_at'] <= pr['merged_at'] and review['state'] != 'COMMENTED':
            reviews[review['user']['id']] = review
    require(any(r['state'] == 'APPROVED' and r['user']['type'] == 'User' and r['commit_id'] == pr['head']['sha'] for r in reviews.values()), 'No human approval for the final PR head')
    manifest = api.repo(f'contents/.release-please-manifest.json?ref={sha}')
    value = json.loads(base64.b64decode(manifest['content']))
    require(value.get('.') == tag[1:], 'Release version differs from manifest')
    return release, {'repository': api.repository, 'release_id': release['id'], 'tag': tag, 'sha': sha, 'pr': pr['number']}


def release_assets(api, release_id):
    result = list(api.pages(f'releases/{release_id}/assets'))
    require(len({a['name'] for a in result}) == len(result), 'Duplicate release assets')
    return {a['name']: a for a in result}


def record_for(api, target, assets, policy):
    if RECORD not in assets:
        return None
    record = json.loads(api.asset(assets[RECORD]))
    require(record.get('schema') == 1, 'Unsupported release record')
    require(all(record.get(key) == value for key, value in target.items()), 'Release record identity mismatch')
    expected = assets_for(policy, target['tag'])
    require(sorted(item['name'] for item in record['assets']) == expected, 'Recorded asset names differ from policy')
    for item in record['assets']:
        require(isinstance(item['size'], int) and item['size'] > 0 and re.fullmatch('[0-9a-f]{64}', item['sha256']), 'Invalid asset record')
    expected_images = containers_for(policy, target['tag'])
    require([(v['destination'], v['archive']) for v in record['containers']] == [(v['destination'], v['archive']) for v in expected_images], 'Recorded image destinations differ from policy')
    for item in record['containers']:
        require(DIGEST.fullmatch(item['digest']) and re.fullmatch('[0-9a-f]{64}', item['archive_sha256']), 'Invalid image digest')
    require(not (set(assets) - set(expected) - {RECORD}), 'Unexpected release attachments')
    return record


def registry_digest(image, tag, token, actor):
    require(re.fullmatch(r'ghcr.io/[a-z0-9_.-]+/[a-z0-9_.-]+', image), 'Invalid image destination')
    repo = image.removeprefix('ghcr.io/')
    query = urllib.parse.urlencode({'service': 'ghcr.io', 'scope': f'repository:{repo}:pull'})
    auth = base64.b64encode(f'{actor}:{token}'.encode()).decode()
    request = urllib.request.Request('https://ghcr.io/token?' + query, headers={'Authorization': 'Basic ' + auth})
    with urllib.request.urlopen(request, timeout=60) as response:
        bearer = json.load(response)['token']
    request = urllib.request.Request(f'https://ghcr.io/v2/{repo}/manifests/{tag}', headers={
        'Authorization': 'Bearer ' + bearer,
        'Accept': 'application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json',
    })
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            data = response.read()
            digest = 'sha256:' + sha256(data)
            require(response.headers.get('Docker-Content-Digest') == digest, 'Registry response digest mismatch')
            return digest
    except urllib.error.HTTPError as error:
        if error.code == 404:
            payload = json.loads(error.read())
            if payload.get('errors') and all(item['code'] in ('MANIFEST_UNKNOWN', 'NAME_UNKNOWN') for item in payload['errors']):
                return None
        raise ValueError(f'Registry inspection failed: HTTP {error.code}') from error


def verify_remote(api, target, record, assets, complete):
    for item in record['assets']:
        asset = assets.get(item['name'])
        if asset is None:
            require(not complete, 'Published release is missing an asset')
            continue
        require(asset['state'] == 'uploaded' and asset['size'] == item['size'], 'Incomplete or changed asset')
        require(sha256(api.asset(asset)) == item['sha256'], 'Published asset digest mismatch')
    for item in record['containers']:
        image, tag = item['destination'].rsplit(':', 1)
        digest = registry_digest(image, tag, api.token, os.environ['GITHUB_ACTOR'])
        require((digest is None and not complete) or digest == item['digest'], 'Published container digest mismatch')


def safe_bundle(data, allowed):
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        entries = archive.infolist()
        require(len(entries) <= 64 and sum(i.file_size for i in entries) <= 8 * 1024**3, 'Artifact exceeds limits')
        names = [item.filename for item in entries if not item.is_dir()]
        require(len(set(names)) == len(names) and set(names) == set(allowed), 'Unexpected artifact files')
        result = {}
        for item in entries:
            if item.is_dir():
                require(item.filename in ('assets/', 'images/'), 'Unexpected artifact directory')
                continue
            require(item.filename in allowed and not PurePosixPath(item.filename).is_absolute() and '..' not in PurePosixPath(item.filename).parts, 'Unsafe artifact path')
            require((item.external_attr >> 16) & 0o170000 != 0o120000, 'Artifact symlink')
            result[item.filename] = archive.read(item)
    return result


def bundle_names(policy, tag):
    return ['assets/' + name for name in assets_for(policy, tag)] + ['images/' + item['archive'] for item in policy.get('containers', [])]


def artifact_bundle(api, artifact, policy, tag, expected_run=None):
    metadata = api.repo(f"actions/artifacts/{int(artifact['id'])}", missing=True)
    if metadata is None or metadata.get('expired'):
        return None
    require(re.fullmatch(r'release-bundle-[1-9][0-9]*', metadata['name']), 'Unexpected artifact name')
    run_id = int(artifact['run_id'])
    require(expected_run is None or expected_run == run_id, 'Artifact is not from this run')
    run = api.repo(f'actions/runs/{run_id}')
    require(run['path'] == WORKFLOW and run['head_branch'] == 'main' and run['event'] in ('push', 'workflow_dispatch') and run['repository']['full_name'] == api.repository and run['head_repository']['full_name'] == api.repository, 'Unexpected artifact workflow origin')
    require(metadata['workflow_run']['id'] == run_id and metadata['workflow_run']['head_sha'] == run['head_sha'], 'Artifact origin mismatch')
    comparison = api.repo(f"compare/{run['head_sha']}...main")
    require(comparison['status'] in ('ahead', 'identical') and comparison['merge_base_commit']['sha'] == run['head_sha'], 'Artifact workflow is outside main history')
    digest = artifact['digest']
    require(DIGEST.fullmatch(digest) and metadata['digest'] == digest, 'Artifact metadata digest mismatch')
    data = api.repo(f"actions/artifacts/{metadata['id']}/zip", binary=True)
    require('sha256:' + sha256(data) == digest, 'Artifact download digest mismatch')
    return safe_bundle(data, bundle_names(policy, tag))


def local_bundle(path, allowed):
    base = Path(path)
    files = {str(p.relative_to(base)): p for p in base.rglob('*') if p.is_file()}
    require(set(files) == set(allowed), 'Unexpected local bundle files')
    require(not any(p.is_symlink() for p in base.rglob('*')), 'Bundle contains symlinks')
    return {name: p.read_bytes() for name, p in files.items()}


def bundle_record(target, policy, bundle, artifact):
    assets = []
    for name in assets_for(policy, target['tag']):
        data = bundle['assets/' + name]
        assets.append({'name': name, 'size': len(data), 'sha256': sha256(data)})
    containers = []
    for item in containers_for(policy, target['tag']):
        path = 'images/' + item['archive']
        with tempfile.NamedTemporaryFile(suffix='.tar') as handle:
            handle.write(bundle[path]); handle.flush()
            raw = subprocess.check_output(['skopeo', 'inspect', '--raw', 'oci-archive:' + handle.name])
        containers.append({'destination': item['destination'], 'archive': item['archive'], 'size': len(bundle[path]), 'archive_sha256': sha256(bundle[path]), 'digest': 'sha256:' + sha256(raw)})
    record = dict(target, schema=1, assets=assets, containers=containers)
    if bundle:
        record['artifact'] = artifact
    return record


def same_payload(existing, proposed):
    return all(existing.get(key) == value for key, value in proposed.items() if key != 'artifact')


def resolve(api, policy, identity, release_id):
    release, target = validate_target(api, release_id, identity)
    assets = release_assets(api, target['release_id'])
    record = record_for(api, target, assets, policy)
    if record is None:
        require(release['draft'] and not assets, 'Unrecorded release already has assets or is public')
        for item in containers_for(policy, target['tag']):
            require(registry_digest(item['image'], target['tag'][1:], api.token, os.environ['GITHUB_ACTOR']) is None, 'Unrecorded container already exists')
    else:
        verify_remote(api, target, record, assets, not release['draft'])
    bundle = None
    if release['draft'] and record and record.get('artifact'):
        bundle = artifact_bundle(api, record['artifact'], policy, target['tag'])
    if bundle is not None:
        for name, data in bundle.items():
            path = Path('bundle') / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
    output('target', target)
    output('sha', target['sha'])
    output('tag', target['tag'])
    output('published', not release['draft'])
    output('bundle', bundle is not None)
    output('build', release['draft'] and bool(policy['assets'] or policy.get('containers')) and bundle is None)


def verify_artifact(api, policy, target, artifact):
    bundle = artifact_bundle(api, artifact, policy, target['tag'], int(os.environ['GITHUB_RUN_ID']))
    require(bundle is not None, 'Current artifact is missing or expired')
    output('hashes', {name: sha256(data) for name, data in bundle.items()})


def advance_latest(api, target, record, policy, update=True):
    releases = [r for r in api.pages('releases') if not r['draft'] and not r['prerelease']]
    stable = []
    for release in releases:
        value, pre = version(release['tag_name'])
        require(not pre, 'Invalid stable release classification')
        stable.append((value, release))
    require(len({value for value, _ in stable}) == len(stable), 'Ambiguous stable versions')
    current = api.repo('releases/latest', missing=True)
    if current is not None:
        current_target = next((r for _, r in stable if r['id'] == current['id']), None)
        require(current_target is not None, 'Latest is not a known stable release')
        current_assets = release_assets(api, current['id'])
        current_sha = tag_sha(api, current['tag_name'])
        current_record = record_for(api, dict(repository=api.repository, release_id=current['id'], tag=current['tag_name'], sha=current_sha, pr=json.loads(api.asset(current_assets[RECORD]))['pr']) if RECORD in current_assets else {}, current_assets, policy)
        require(current_record is not None, 'Latest has no trusted publication record')
        verify_remote(api, {}, current_record, current_assets, True)
    known = {}
    for _, release in stable:
        assets = release_assets(api, release['id'])
        if RECORD in assets:
            candidate = json.loads(api.asset(assets[RECORD]))
            require(candidate.get('repository') == api.repository and candidate.get('release_id') == release['id'] and candidate.get('tag') == release['tag_name'], 'Invalid historical record')
            for item in candidate.get('containers', []):
                known.setdefault(item['destination'].rsplit(':', 1)[0], set()).add(item['digest'])
    if not update:
        for item in policy.get('containers', []):
            digest = registry_digest(item['image'], 'latest', api.token, os.environ['GITHUB_ACTOR'])
            require(digest is None or digest in known.get(item['image'], set()), 'Container latest has an unknown digest')
        return
    if version(target['tag'])[1] or not stable or max(value for value, _ in stable) != version(target['tag'])[0]:
        return
    for item in record['containers']:
        image = item['destination'].rsplit(':', 1)[0]
        latest = registry_digest(image, 'latest', api.token, os.environ['GITHUB_ACTOR'])
        require(latest is None or latest in known.get(image, set()), 'Container latest has an unknown digest')
        if latest != item['digest']:
            subprocess.run(['skopeo', 'copy', '--all', '--preserve-digests', 'docker://' + image + '@' + item['digest'], 'docker://' + image + ':latest'], check=True)
        require(registry_digest(image, 'latest', api.token, os.environ['GITHUB_ACTOR']) == item['digest'], 'Latest image verification failed')
    if current is None or current['id'] != target['release_id']:
        api.repo(f"releases/{target['release_id']}", method='PATCH', data={'make_latest': 'true'})
    require(api.repo('releases/latest')['id'] == target['release_id'], 'Latest release verification failed')


def publish(api, policy, identity, target, artifact, hashes):
    release, verified = validate_target(api, target['release_id'], identity)
    require(verified == target, 'Release target changed before publishing')
    assets = release_assets(api, target['release_id'])
    record = record_for(api, target, assets, policy)
    if not release['draft']:
        require(record is not None, 'Published release has no record')
        verify_remote(api, target, record, assets, True)
        advance_latest(api, target, record, policy)
        return
    bundle = local_bundle('bundle', bundle_names(policy, target['tag'])) if bundle_names(policy, target['tag']) else {}
    require({name: sha256(data) for name, data in bundle.items()} == hashes, 'Downloaded artifact content differs from verified artifact')
    proposed = bundle_record(target, policy, bundle, artifact)
    advance_latest(api, target, proposed, policy, update=False)
    if record is not None:
        require(same_payload(record, proposed), 'Rebuilt artifacts differ from the publication record')
        verify_remote(api, target, record, assets, False)
    else:
        require(not assets, 'Unrecorded draft is not empty')
        for item in containers_for(policy, target['tag']):
            require(registry_digest(item['image'], target['tag'][1:], api.token, os.environ['GITHUB_ACTOR']) is None, 'Unrecorded container already exists')
        record = proposed
        upload_asset(api, target['release_id'], RECORD, json.dumps(record, sort_keys=True, indent=2).encode() + b'\n')
    for item in record['assets']:
        if item['name'] not in assets:
            upload_asset(api, target['release_id'], item['name'], bundle['assets/' + item['name']])
    for item in record['containers']:
        image, tag = item['destination'].rsplit(':', 1)
        current = registry_digest(image, tag, api.token, os.environ['GITHUB_ACTOR'])
        require(current is None or current == item['digest'], 'Versioned image already has another digest')
        if current is None:
            subprocess.run(['skopeo', 'copy', '--all', '--preserve-digests', 'oci-archive:bundle/images/' + item['archive'], 'docker://' + item['destination']], check=True)
    verify_remote(api, target, record, release_assets(api, target['release_id']), True)
    require(tag_sha(api, target['tag']) == target['sha'], 'Tag changed during publication')
    api.repo(f"releases/{target['release_id']}", method='PATCH', data={'draft': False, 'make_latest': 'false'})
    advance_latest(api, target, record, policy)


def upload_asset(api, release_id, name, data):
    require(re.fullmatch('[A-Za-z0-9_.-]+', name), 'Invalid asset name')
    api.request(f'repos/{api.repository}/releases/{release_id}/assets?name={urllib.parse.quote(name)}', method='POST', data=data, upload=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('command', choices=['resolve', 'verify-artifact', 'publish'])
    args = parser.parse_args()
    require(os.environ['GITHUB_REF'] == 'refs/heads/main', 'Release workflow must run on main')
    policy = json.loads(Path(__file__).with_name('release-policy.json').read_text())
    api = GitHub(os.environ['GITHUB_REPOSITORY'], os.environ['GH_TOKEN'])
    identity = {'bot_id': os.environ['AUTOMATION_APP_BOT_ID'], 'bot_login': os.environ['AUTOMATION_APP_BOT_LOGIN']}
    require(identity['bot_id'].isdigit() and identity['bot_login'].endswith('[bot]'), 'Configure the dedicated App bot identity')
    if args.command == 'resolve':
        resolve(api, policy, identity, os.environ['RELEASE_ID'])
        return
    target = json.loads(os.environ['TARGET'])
    raw_digest = os.environ.get('ARTIFACT_DIGEST', '')
    artifact = {'id': os.environ.get('ARTIFACT_ID', ''), 'digest': artifact_digest(raw_digest) if raw_digest else '', 'run_id': int(os.environ['GITHUB_RUN_ID'])}
    if args.command == 'verify-artifact':
        verify_artifact(api, policy, target, artifact)
    else:
        publish(api, policy, identity, target, artifact, json.loads(os.environ.get('BUNDLE_HASHES') or '{}'))


if __name__ == '__main__':
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError, urllib.error.URLError, zipfile.BadZipFile) as error:
        print(f'::error::{error}', file=sys.stderr)
        sys.exit(1)
