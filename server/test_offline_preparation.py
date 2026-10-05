"""Small synthetic preparation fixtures: no bulk provider downloads or services."""

import copy
import gzip
import hashlib
import io
import json
import sqlite3
import tempfile
import unittest
import urllib.error
import urllib.request
import urllib.response
from email.message import Message
from pathlib import Path
from unittest.mock import patch

import prepare_providers as preparer


def digest(data):
    return hashlib.sha256(data).hexdigest()


def synthetic_provider(root, *, native_id=False):
    repository = 'example/fixture'
    provider_root = root / repository
    provider_root.mkdir(parents=True)
    database_path = provider_root / 'native.sqlite'
    connection = sqlite3.connect(database_path)
    key = 'id' if native_id else 'code'
    connection.execute(f'CREATE TABLE records ({key} TEXT PRIMARY KEY, value BLOB)')
    connection.execute('INSERT INTO records VALUES (?, ?)', ('https://example.test/a% b', b'\x00\xff'))
    connection.commit()
    connection.close()
    artifact = database_path.read_bytes()
    artifact_pin = {'path': 'native.sqlite', 'bytes': len(artifact), 'sha256': digest(artifact)}
    source = {'repository': 'https://github.com/example/source', 'revision': 'b' * 40, 'path': 'native.sqlite', 'sha256': digest(artifact), 'license': 'CC0-1.0', 'licenseFile': 'LICENSE.txt'}
    manifest = {'id': 'fixture', 'siteHost': 'fixture.example.test', 'source': source, 'capabilities': {'ovdb': {'readOnly': True, 'available': True, 'query': True, 'canonicalUrl': 'https://fixture.example.test/db/fixture/'}}}
    contract = {'contractVersion': 1, 'manifest': {'id': 'fixture'}, 'exports': [{'format': 'sqlite', **artifact_pin}], 'schema': {'database': {'id': 'fixture'}, 'tables': [{'name': 'records', 'rowCount': 1, 'columns': [{'name': key, 'type': 'TEXT'}, {'name': 'value', 'type': 'BLOB'}]}]}}
    checksums = {'contractVersion': 1, 'files': {'native.sqlite': artifact_pin}}
    descriptor = {'format': 'ovdb-database/draft-1', 'localId': 'fixture', 'id': 'https://fixture.example.test/db/fixture/', 'serverId': 'https://fixture.example.test/', 'apiUrl': 'https://fixture.example.test/v1/databases/fixture', 'serverDbBaseUrl': 'https://fixture.example.test/db/fixture/', 'homepage': 'https://fixture.example.test/', 'deployment': {'engine': 'sqlite'}, 'capabilities': {'read': True, 'query': True, 'write': False}, 'licences': {'data': source['license']}, 'provenance': {**source}}
    files = {'manifest': ('manifest.json', json.dumps(manifest).encode()), 'contract': ('contract.json', json.dumps(contract).encode()), 'checksums': ('checksums.json', json.dumps(checksums).encode()), 'databaseManifest': ('ovdb-database.json', json.dumps(descriptor).encode()), 'license': ('LICENSE.txt', b'CC0 fixture rights\n')}
    pins = {'artifact': artifact_pin}
    for field, (name, content) in files.items():
        (provider_root / name).write_bytes(content)
        pins[field] = {'path': name, 'bytes': len(content), 'sha256': digest(content)}
    return {'id': 'fixture', 'repository': repository, 'revision': 'a' * 40, 'recordKeyFormat': 'natural', 'corsOrigins': ['https://fixture.example.test'], 'files': pins}


def compressed_provider(decoded, *, members=False):
    encoded = gzip.compress(decoded[:len(decoded)//2], mtime=0) + gzip.compress(decoded[len(decoded)//2:], mtime=0) if members else gzip.compress(decoded, mtime=0)
    split = len(encoded)//2
    contents = {'native.sqlite.gz.part-1': encoded[:split], 'native.sqlite.gz.part-2': encoded[split:]}
    chunks = [{'path': name, 'bytes': len(data), 'sha256': digest(data)} for name, data in contents.items()]
    artifact = {'path': 'native.sqlite', 'compression': 'gzip', 'encodedPath': 'native.sqlite.gz', 'bytes': len(encoded), 'sha256': digest(encoded), 'decodedBytes': len(decoded), 'decodedSha256': digest(decoded), 'chunks': chunks}
    provider = {'id': 'fixture', 'repository': 'example/fixture', 'revision': 'a' * 40, 'files': {'artifact': artifact}}
    checksums = {'files': {chunk['path']: chunk for chunk in chunks}}
    return provider, checksums, contents


class OfflinePreparationTest(unittest.TestCase):
    def test_version_two_is_opt_in_and_version_one_output_stays_legacy(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            provider = synthetic_provider(root)
            inventory = root / 'providers.json'
            inventory.write_text(json.dumps({'version': 1, 'databases': [provider]}))
            runtime_path = preparer.prepare_inventory(inventory, root / 'v1', root)
            runtime = json.loads(runtime_path.read_bytes())
            self.assertEqual(1, runtime['version'])
            self.assertNotIn('manifestSha256', runtime['databases'][0])
            self.assertNotIn('sqlite:', (root / 'v1/fixture.yaml').read_text())
            old_bytes = runtime_path.read_bytes()
            inventory.write_text(json.dumps({'version': 2, 'databases': [provider]}))
            same = preparer.prepare_inventory(inventory, root / 'v2-default', root)
            self.assertEqual((root / 'v1/fixture.yaml').read_bytes(), (root / 'v2-default/fixture.yaml').read_bytes())
            self.assertEqual((root / 'v1/fixture.sqlite').read_bytes(), (root / 'v2-default/fixture.sqlite').read_bytes())
            self.assertEqual(2, json.loads(same.read_bytes())['version'])
            provider.update(servingAdapter='separate-id/1', readProfile='bounded-immutable/1')
            inventory.write_text(json.dumps({'version': 2, 'databases': [provider]}))
            opt_in = preparer.prepare_inventory(inventory, root / 'opt-in', root)
            entry = json.loads(opt_in.read_bytes())['databases'][0]
            self.assertEqual(digest((root / 'opt-in/fixture.yaml').read_bytes()), entry['manifestSha256'])
            self.assertEqual('separate-id/1', entry['servingAdapter'])
            self.assertEqual('bounded-immutable/1', entry['readProfile'])
            self.assertNotIn('record_keys', entry)
            self.assertEqual(old_bytes, runtime_path.read_bytes())
            self.assertFalse(list((root / 'opt-in').glob('*-source-*')))
            provider = synthetic_provider(root / 'with-id', native_id=True)
            provider.update(servingAdapter='separate-id/1', readProfile='bounded-immutable/1')
            inventory.write_text(json.dumps({'version': 2, 'databases': [provider]}))
            preparer.prepare_inventory(inventory, root / 'native-id', root / 'with-id')
            serving = sqlite3.connect(root / 'native-id/fixture.sqlite')
            self.assertEqual(('https://example.test/a% b', b'\x00\xff'), serving.execute('SELECT id, value FROM records').fetchone())
            serving.close()

    def test_inventory_versions_and_fields_are_closed_before_fetch(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            provider = synthetic_provider(root)
            inventory = root / 'providers.json'
            cases = [({'version': value, 'databases': [provider]}, 'version') for value in (True, 0, 3, '2')]
            cases += [({'version': 2, 'databases': [provider], 'extra': True}, 'version')]
            for field, value in [('servingAdapter', 'other'), ('readProfile', None), ('unknown', True), ('attribution', []), ('publicRouteBindings', {})]:
                cases.append(({'version': 2, 'databases': [{**provider, field: value}]}, 'unsupported|unknown'))
            cases.append(({'version': 1, 'databases': [{**provider, 'servingAdapter': 'separate-id/1'}]}, 'unknown'))
            unknown_file = copy.deepcopy(provider)
            unknown_file['files']['manifest']['url'] = 'https://evil.test/file'
            cases.append(({'version': 2, 'databases': [unknown_file]}, 'unknown'))
            for document, error in cases:
                with self.subTest(document=document):
                    inventory.write_text(json.dumps(document))
                    with patch.object(preparer, 'default_fetch') as fetch, self.assertRaisesRegex(ValueError, error):
                        preparer.prepare_inventory(inventory, root / 'out')
                    fetch.assert_not_called()

    def test_immutable_url_rejects_path_and_authority_confusion_before_fetch(self):
        url = preparer.immutable_url('example/fixture', 'a'*40, 'artifacts/a space/ü.sqlite')
        self.assertEqual('https://raw.githubusercontent.com/example/fixture/' + 'a'*40 + '/artifacts/a%20space/%C3%BC.sqlite', url)
        bad_paths = ['../secret', 'a/../b', '/abs', 'a//b', './a', 'a/./b', 'a/', r'a\b', 'a?x', 'a#x', '%2e%2e/secret', 'a%2fb', 'https://evil.test/x', 'a\nheader', 'a\x00b']
        with patch.object(urllib.request, 'build_opener') as opener:
            for path in bad_paths:
                with self.subTest(path=path), self.assertRaises(ValueError):
                    preparer.default_fetch('example/fixture', 'a'*40, path)
            for repository in ('../fixture', 'example/..', 'example/fixture/extra', 'user@example/fixture', 'https://evil.test/repo'):
                with self.subTest(repository=repository), self.assertRaises(ValueError):
                    preparer.default_fetch(repository, 'a'*40, 'x')
            with self.assertRaises(ValueError):
                preparer.default_fetch('example/fixture', 'main', 'x')
            opener.assert_not_called()

    def test_all_redirects_are_refused_before_any_second_request(self):
        initial = preparer.immutable_url('example/fixture', 'a'*40, 'fixture.sqlite')
        for status in (301, 302, 303, 307, 308):
            for location in (initial + '.other', 'https://evil.test/x', '/same-origin'):
                seen = []
                responses = []
                def https_open(_handler, request):
                    seen.append(request.full_url)
                    headers = Message()
                    headers['Location'] = location
                    response = urllib.response.addinfourl(io.BytesIO(b'ignored'), headers, request.full_url, status)
                    response.msg = 'redirect'
                    responses.append(response)
                    return response
                with self.subTest(status=status, location=location), patch.object(urllib.request.HTTPSHandler, 'https_open', https_open), self.assertRaisesRegex(ValueError, 'redirect refused'):
                    preparer.default_fetch('example/fixture', 'a'*40, 'fixture.sqlite')
                self.assertEqual([initial], seen)
                self.assertTrue(all(response.closed for response in responses))

    def test_fetch_limits_success_and_error_bodies(self):
        class Body(io.BytesIO):
            def read(self, amount=-1):
                self.amount = amount
                return super().read(amount)
        class Response(Body):
            def __enter__(self): return self
            def __exit__(self, *_): pass
        for size in (8, 9):
            response = Response(b'x' * size)
            with patch.object(preparer, 'MAX_ARTIFACT_BYTES', 8), patch.object(urllib.request.OpenerDirector, 'open', return_value=response):
                if size == 8:
                    self.assertEqual(b'x'*8, preparer.default_fetch('example/fixture', 'a'*40, 'x'))
                else:
                    with self.assertRaisesRegex(ValueError, 'build limit'):
                        preparer.default_fetch('example/fixture', 'a'*40, 'x')
            self.assertEqual(9, response.amount)
        body = Body(b'untrusted error' * 100)
        error = urllib.error.HTTPError('https://raw.githubusercontent.com/x', 500, 'server', {}, body)
        with patch.object(urllib.request.OpenerDirector, 'open', side_effect=error), self.assertRaisesRegex(ValueError, '^immutable provider request failed: HTTP 500$'):
            preparer.default_fetch('example/fixture', 'a'*40, 'x')
        self.assertTrue(body.closed)
        self.assertFalse(hasattr(body, 'amount'))

    def test_ordered_stream_fragments_and_gzip_members_with_exact_limits(self):
        for members in (False, True):
            decoded = b'original\x00\xff data' * 100
            provider, checksums, contents = compressed_provider(decoded, members=members)
            artifact = provider['files']['artifact']
            with tempfile.TemporaryDirectory() as temporary:
                output = Path(temporary) / 'decoded.sqlite'
                with patch.object(preparer, 'MAX_ARTIFACT_BYTES', max(map(len, contents.values()))), patch.object(preparer, 'MAX_ENCODED_ARTIFACT_BYTES', artifact['bytes']), patch.object(preparer, 'MAX_DECODED_ARTIFACT_BYTES', len(decoded)):
                    preparer._fetch_verified_artifact(provider, lambda _repo, _rev, path: contents[path], checksums, output)
                self.assertEqual(decoded, output.read_bytes())
                self.assertEqual([output], list(Path(temporary).iterdir()))

    def test_bad_chunk_order_corruption_missing_truncation_expansion_and_cleanup(self):
        decoded = b'expand me\x00' * 500
        provider, checksums, contents = compressed_provider(decoded)
        variants = []
        reversed_chunks = copy.deepcopy(provider)
        reversed_chunks['files']['artifact']['chunks'].reverse()
        variants.append((reversed_chunks, checksums, contents, None))
        variants.append((provider, checksums, {name: b'wrong' for name in contents}, None))
        variants.append((provider, checksums, {}, None))
        truncated, truncated_checksums, truncated_contents = compressed_provider(decoded)
        last = list(truncated_contents)[-1]
        truncated_contents[last] = truncated_contents[last][:-4]
        last_pin = truncated['files']['artifact']['chunks'][-1]
        last_pin.update(bytes=len(truncated_contents[last]), sha256=digest(truncated_contents[last]))
        encoded = b''.join(truncated_contents.values())
        truncated['files']['artifact'].update(bytes=len(encoded), sha256=digest(encoded))
        variants.append((truncated, truncated_checksums, truncated_contents, None))
        expansion = copy.deepcopy(provider)
        expansion['files']['artifact']['decodedBytes'] = len(decoded)-1
        variants.append((expansion, checksums, contents, None))
        variants.append((provider, checksums, contents, ('MAX_ENCODED_ARTIFACT_BYTES', provider['files']['artifact']['bytes']-1)))
        variants.append((provider, checksums, contents, ('MAX_DECODED_ARTIFACT_BYTES', len(decoded)-1)))
        variants.append((provider, checksums, contents, ('MAX_ARTIFACT_BYTES', max(map(len, contents.values()))-1)))
        for candidate, advertised, fetched, bound in variants:
            with self.subTest(bound=bound, candidate=candidate), tempfile.TemporaryDirectory() as temporary:
                output = Path(temporary) / 'decoded.sqlite'
                context = patch.object(preparer, *bound) if bound else patch.object(preparer, 'MAX_DECODED_ARTIFACT_BYTES', preparer.MAX_DECODED_ARTIFACT_BYTES)
                with context, self.assertRaises((ValueError, KeyError, EOFError, OSError)):
                    preparer._fetch_verified_artifact(candidate, lambda _repo, _rev, path: fetched[path], advertised, output)
                self.assertEqual([], list(Path(temporary).iterdir()))

    def test_inventory_duplicate_missing_chunks_and_boundaries(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            base = synthetic_provider(root)
            inventory = root / 'providers.json'
            compressed, _, _ = compressed_provider(b'fixture')
            base['files']['artifact'] = compressed['files']['artifact']
            inventory.write_text(json.dumps({'version': 2, 'databases': [base]}))
            artifact = base['files']['artifact']
            with patch.object(preparer, 'MAX_ARTIFACT_BYTES', max(chunk['bytes'] for chunk in artifact['chunks'])), patch.object(preparer, 'MAX_ENCODED_ARTIFACT_BYTES', artifact['bytes']), patch.object(preparer, 'MAX_DECODED_ARTIFACT_BYTES', artifact['decodedBytes']):
                # Metadata pins are independently bounded too, so omit their optional byte counts.
                for name, pin in base['files'].items():
                    if name != 'artifact': pin.pop('bytes', None)
                inventory.write_text(json.dumps({'version': 2, 'databases': [base]}))
                preparer.load_inventory(inventory)
            for change in ('duplicate', 'missing', 'unknown', 'decoded', 'encoded', 'physical'):
                candidate = copy.deepcopy(base)
                pin = candidate['files']['artifact']
                if change == 'duplicate': pin['chunks'].append(pin['chunks'][0])
                if change == 'missing': pin['chunks'].pop()
                if change == 'unknown': pin['chunks'][0]['url'] = 'https://evil.test'
                if change == 'decoded': pin['decodedBytes'] = preparer.MAX_DECODED_ARTIFACT_BYTES + 1
                if change == 'encoded': pin['bytes'] = preparer.MAX_ENCODED_ARTIFACT_BYTES + 1
                if change == 'physical': pin['chunks'][0]['bytes'] = preparer.MAX_ARTIFACT_BYTES + 1
                inventory.write_text(json.dumps({'version': 2, 'databases': [candidate]}))
                with self.subTest(change=change), self.assertRaises(ValueError):
                    preparer.load_inventory(inventory)

    def test_local_symlinks_cannot_escape_repository(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            provider = synthetic_provider(root)
            outside = root / 'outside.json'
            outside.write_text('{}')
            manifest = root / provider['repository'] / provider['files']['manifest']['path']
            manifest.unlink()
            manifest.symlink_to(outside)
            inventory = root / 'providers.json'
            inventory.write_text(json.dumps({'version': 1, 'databases': [provider]}))
            with self.assertRaisesRegex(ValueError, 'escapes its repository'):
                preparer.prepare_inventory(inventory, root / 'out', root)
            self.assertEqual([], list((root / 'out').iterdir()))


if __name__ == '__main__':
    unittest.main()
