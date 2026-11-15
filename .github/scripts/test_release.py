import base64
import copy
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import zipfile

import release as r

SHA = 'a' * 40
HEAD = 'b' * 40
TARGET = {'repository': 'owner/project', 'release_id': 12, 'tag': 'v1.2.3', 'sha': SHA, 'pr': 7}
IDENTITY = {'bot_id': '100', 'bot_login': 'automation[bot]'}
POLICY = {'assets': ['tool_{version}_linux_amd64', 'checksums.txt'], 'containers': []}


class API:
    repository = 'owner/project'
    token = 'test'

    def __init__(self):
        self.release = {'id': 12, 'tag_name': 'v1.2.3', 'draft': True, 'prerelease': False}
        self.pr = {'number': 7, 'merged_at': '2026-01-02T00:00:00Z', 'merge_commit_sha': SHA, 'base': {'ref': 'main', 'repo': {'full_name': self.repository}}, 'head': {'ref': 'release-please--branches--main', 'sha': HEAD, 'repo': {'full_name': self.repository}}, 'user': {'type': 'Bot', 'id': 100, 'login': 'automation[bot]'}}
        self.reviews = [{'state': 'APPROVED', 'submitted_at': '2026-01-01T00:00:00Z', 'commit_id': HEAD, 'user': {'id': 10, 'type': 'User'}}]
        self.comparison = {'status': 'ahead', 'merge_base_commit': {'sha': SHA}}
        self.assets = {}
        self.record = None
        self.calls = []
        self.releases = []

    def repo(self, path, **kwargs):
        self.calls.append((path, kwargs))
        if path == 'releases/12': return copy.deepcopy(self.release)
        if path == 'git/ref/tags/v1.2.3': return {'object': {'type': 'commit', 'sha': SHA}}
        if path.startswith('compare/'): return self.comparison
        if path == 'pulls/7': return self.pr
        if path.startswith('contents/'): return {'content': base64.b64encode(b'{".": "1.2.3"}').decode()}
        if path == 'releases/latest': return None
        if path == 'actions/artifacts/20': return self.artifact
        if path == 'actions/runs/30': return self.run
        if path == 'actions/artifacts/20/zip': return self.zip
        raise AssertionError(path)

    def pages(self, path):
        if path.startswith('commits/'): return [self.pr]
        if path.endswith('/reviews'): return self.reviews
        if path.endswith('/assets'): return list(self.assets.values())
        if path == 'releases': return self.releases
        raise AssertionError(path)

    def asset(self, asset):
        return self.record if asset['name'] == r.RECORD else b'actual'


class TargetTests(unittest.TestCase):
    def test_merged_app_pr_with_final_human_review(self):
        self.assertEqual(r.validate_target(API(), 12, IDENTITY)[1], TARGET)

    def test_recovery_rejects_untrusted_targets(self):
        for field in ['bot', 'fork', 'base', 'head', 'merge', 'review', 'history', 'version']:
            api = API()
            if field == 'bot': api.pr['user']['id'] = 999
            if field == 'fork': api.pr['head']['repo']['full_name'] = 'fork/project'
            if field == 'base': api.pr['base']['ref'] = 'other'
            if field == 'head': api.reviews[0]['commit_id'] = 'c' * 40
            if field == 'merge': api.pr['merge_commit_sha'] = HEAD
            if field == 'review': api.reviews[0]['user']['type'] = 'Bot'
            if field == 'history': api.comparison['status'] = 'diverged'
            if field == 'version': api.release['tag_name'] = '../main'
            with self.subTest(field=field), self.assertRaises(ValueError):
                r.validate_target(api, 12, IDENTITY)

    def test_later_dismissal_invalidates_approval(self):
        api = API(); review = copy.deepcopy(api.reviews[0])
        review['state'] = 'DISMISSED'; api.reviews.append(review)
        with self.assertRaises(ValueError): r.validate_target(api, 12, IDENTITY)

    def test_review_after_merge_does_not_authorize_recovery(self):
        api = API(); api.reviews[0]['submitted_at'] = '2026-01-03T00:00:00Z'
        with self.assertRaises(ValueError): r.validate_target(api, 12, IDENTITY)


class ArtifactTests(unittest.TestCase):
    def test_downloads_use_the_media_type_of_each_api(self):
        api = r.GitHub('owner/project', 'test')
        with patch.object(api.opener, 'open') as opened:
            opened.return_value.__enter__.return_value.read.return_value = b'data'
            api.repo('actions/artifacts/1/zip', binary=True)
            self.assertEqual(opened.call_args.args[0].get_header('Accept'), 'application/vnd.github+json')
            api.repo('releases/assets/2', binary=True)
            self.assertEqual(opened.call_args.args[0].get_header('Accept'), 'application/octet-stream')

    def test_cross_host_redirects_strip_credentials(self):
        req = r.urllib.request.Request('https://api.github.com/archive', headers={'Authorization': 'Bearer test'})
        redirected = r.Redirect().redirect_request(req, None, 302, '', {}, 'https://objects.example/archive')
        self.assertIsNone(redirected.get_header('Authorization'))

    def test_action_hash_and_api_digest_share_one_format(self):
        self.assertEqual(r.artifact_digest('a' * 64), 'sha256:' + 'a' * 64)
        self.assertEqual(r.artifact_digest('sha256:' + 'a' * 64), 'sha256:' + 'a' * 64)
        for value in ['', 'sha256:short', 'sha1:' + 'a' * 64]:
            with self.assertRaises(ValueError): r.artifact_digest(value)

    def archive(self, entries):
        data = io.BytesIO()
        with zipfile.ZipFile(data, 'w') as archive:
            for name, value in entries: archive.writestr(name, value)
        return data.getvalue()

    def test_zip_rejects_path_traversal_and_extra_files(self):
        for name in ['../execute', '/absolute', 'assets/execute', 'images/execute']:
            with self.subTest(name=name), self.assertRaises(ValueError):
                r.safe_bundle(self.archive([(name, b'bad')]), ['assets/tool'])

    def test_zip_rejects_symlinks(self):
        data = io.BytesIO()
        with zipfile.ZipFile(data, 'w') as archive:
            item = zipfile.ZipInfo('assets/tool'); item.external_attr = 0o120777 << 16
            archive.writestr(item, '../target')
        with self.assertRaises(ValueError): r.safe_bundle(data.getvalue(), ['assets/tool'])

    def test_download_digest_is_enforced(self):
        api = API()
        api.zip = self.archive([('assets/tool_1.2.3_linux_amd64', b'tool'), ('assets/checksums.txt', b'checksums')])
        digest = 'sha256:' + r.sha256(api.zip)
        api.artifact = {'name': 'release-bundle-1', 'id': 20, 'digest': digest, 'expired': False, 'workflow_run': {'id': 30, 'head_sha': SHA}}
        api.run = {'path': r.WORKFLOW, 'head_branch': 'main', 'head_sha': SHA, 'event': 'push', 'repository': {'full_name': api.repository}, 'head_repository': {'full_name': api.repository}}
        expected = {'id': 20, 'run_id': 30, 'digest': digest}
        self.assertEqual(len(r.artifact_bundle(api, expected, POLICY, 'v1.2.3')), 2)
        for key, value in [('path', '.github/workflows/ci.yml'), ('head_branch', 'untrusted'), ('head_sha', HEAD), ('head_repository', {'full_name': 'fork/project'})]:
            previous = api.run[key]
            api.run[key] = value
            with self.subTest(origin=key), self.assertRaises(ValueError):
                r.artifact_bundle(api, expected, POLICY, 'v1.2.3')
            api.run[key] = previous
        with self.assertRaises(ValueError): r.artifact_bundle(api, expected, POLICY, 'v1.2.3', expected_run=31)
        api.zip += b'corruption'
        with self.assertRaises(ValueError): r.artifact_bundle(api, expected, POLICY, 'v1.2.3')

    def test_local_bundle_rejects_unexpected_files(self):
        with tempfile.TemporaryDirectory() as directory:
            (Path(directory) / 'command.sh').write_bytes(b'bad')
            with self.assertRaises(ValueError): r.local_bundle(directory, ['assets/tool'])


class PublicationTests(unittest.TestCase):
    def test_rebuild_can_change_run_but_not_bytes(self):
        old = dict(TARGET, schema=1, assets=[{'name': 'tool', 'sha256': 'old'}], containers=[], artifact={'run_id': 1})
        new = dict(old, artifact={'run_id': 2})
        self.assertTrue(r.same_payload(old, new))
        new = copy.deepcopy(new); new['assets'][0]['sha256'] = 'new'
        self.assertFalse(r.same_payload(old, new))

    def test_remote_mismatch_and_incomplete_assets_stop_publication(self):
        for size, state, digest in [(6, 'uploaded', '0' * 64), (5, 'uploaded', '0' * 64), (6, 'starter', r.sha256(b'actual'))]:
            api = API(); record = {'assets': [{'name': 'tool', 'size': size, 'sha256': digest}], 'containers': []}
            assets = {'tool': {'name': 'tool', 'state': state, 'size': 6}}
            with self.subTest(state=state, size=size), self.assertRaises(ValueError):
                r.verify_remote(api, TARGET, record, assets, True)

    def test_missing_asset_only_allowed_for_draft(self):
        record = {'assets': [{'name': 'tool'}], 'containers': []}
        r.verify_remote(API(), TARGET, record, {}, False)
        with self.assertRaises(ValueError): r.verify_remote(API(), TARGET, record, {}, True)

    def test_published_release_cannot_gain_a_record(self):
        api = API(); api.release['draft'] = False
        with self.assertRaises(ValueError): r.publish(api, POLICY, IDENTITY, TARGET, {}, {})
        self.assertFalse(any(call[1].get('method') in ('POST', 'PATCH') for call in api.calls))

    def test_older_version_never_changes_latest(self):
        api = API(); api.releases = [{'id': 99, 'tag_name': 'v1.10.0', 'draft': False, 'prerelease': False}]
        r.advance_latest(api, TARGET, {'containers': []}, POLICY)
        self.assertFalse(any(call[1].get('method') == 'PATCH' for call in api.calls))

    def test_semver_is_numeric_and_prerelease_is_distinct(self):
        self.assertGreater(r.version('v1.10.0')[0], r.version('v1.9.9')[0])
        self.assertEqual(r.version('v1.2.3-rc.1')[1], 'rc.1')
        for tag in ['v01.2.3', '1.2.3', 'v1.2.3-01', 'v1.2.3/evil']:
            with self.assertRaises(ValueError): r.version(tag)



class PublishingAPI(API):
    def __init__(self):
        super().__init__()
        self.data = {}
        self.uploads = []
        self.latest = None

    def repo(self, path, **kwargs):
        if path == 'releases/latest': return self.latest
        if path == 'releases/12' and kwargs.get('method') == 'PATCH':
            self.calls.append((path, kwargs))
            self.release.update(kwargs['data'])
            if kwargs['data'].get('make_latest') == 'true': self.latest = copy.deepcopy(self.release)
            return self.release
        return super().repo(path, **kwargs)

    def pages(self, path):
        if path == 'releases': return [] if self.release['draft'] else [self.release]
        return super().pages(path)

    def request(self, path, **kwargs):
        name = path.split('?name=')[1]
        self.uploads.append(name)
        self.data[name] = kwargs['data']
        self.assets[name] = {'id': len(self.assets) + 1, 'name': name, 'size': len(kwargs['data']), 'state': 'uploaded'}

    def asset(self, asset): return self.data[asset['name']]


class PublicationFlowTests(unittest.TestCase):
    def test_source_release_records_before_publishing_and_retry_does_not_write(self):
        api = PublishingAPI(); policy = {'assets': [], 'containers': []}
        r.publish(api, policy, IDENTITY, TARGET, {}, {})
        self.assertEqual(api.uploads, [r.RECORD])
        self.assertFalse(api.release['draft'])
        self.assertEqual(api.latest['id'], 12)
        self.assertNotIn('artifact', json.loads(api.data[r.RECORD]))
        before = len(api.calls)
        r.publish(api, policy, IDENTITY, TARGET, {}, {})
        self.assertFalse(any(kw.get('method') in ('PATCH', 'POST') for _, kw in api.calls[before:]))
        self.assertEqual(api.uploads, [r.RECORD])

    def test_binary_publication_resumes_only_missing_assets(self):
        api = PublishingAPI()
        bundle = {'assets/tool_1.2.3_linux_amd64': b'binary', 'assets/checksums.txt': b'checksums'}
        artifact = {'id': '20', 'run_id': 30, 'digest': 'sha256:' + '0' * 64}
        record = r.bundle_record(TARGET, POLICY, bundle, artifact)
        r.upload_asset(api, 12, r.RECORD, json.dumps(record).encode())
        r.upload_asset(api, 12, 'tool_1.2.3_linux_amd64', b'binary')
        api.uploads.clear()
        with patch.object(r, 'local_bundle', return_value=bundle):
            r.publish(api, POLICY, IDENTITY, TARGET, artifact, {key: r.sha256(data) for key, data in bundle.items()})
        self.assertEqual(api.uploads, ['checksums.txt'])
        self.assertFalse(api.release['draft'])

    def test_hash_mismatch_never_writes_a_record(self):
        api = PublishingAPI()
        bundle = {'assets/tool_1.2.3_linux_amd64': b'binary', 'assets/checksums.txt': b'checksums'}
        with patch.object(r, 'local_bundle', return_value=bundle), self.assertRaises(ValueError):
            r.publish(api, POLICY, IDENTITY, TARGET, {}, {})
        self.assertEqual(api.uploads, [])
        self.assertTrue(api.release['draft'])


class LatestPreflightTests(unittest.TestCase):
    def test_unknown_latest_stops_before_the_first_upload(self):
        api = PublishingAPI()
        api.latest = {'id': 999, 'tag_name': 'v0.1.0', 'draft': False, 'prerelease': False}
        with self.assertRaises(ValueError): r.publish(api, {'assets': [], 'containers': []}, IDENTITY, TARGET, {}, {})
        self.assertEqual(api.uploads, [])
        self.assertTrue(api.release['draft'])


if __name__ == '__main__': unittest.main()
