"""Pure-clock and fake-HTTP regressions for the production smoke controller."""
import io
import json
import tempfile
from pathlib import Path
import unittest
from email.message import Message
from urllib.error import HTTPError
from unittest.mock import patch

import smoke_inventory as smoke


class Clock:
    def __init__(self):
        self.now = 0
        self.waits = []

    def sleep(self, delay):
        self.waits.append(delay)
        self.now += delay


class Response(io.BytesIO):
    def __init__(self, body=b'original data'):
        super().__init__(body)
        self.status = 200
        self.headers = Message()


class SmokeRequestPolicyTest(unittest.TestCase):
    def setUp(self):
        smoke.OBSERVATIONS.clear()
        self.clock = Clock()
        self.calls = []

    def policy(self, outcomes=None, **kwargs):
        outcomes = iter(outcomes or [Response()])
        def transport(request, timeout):
            self.calls.append((request, self.clock.now, timeout))
            result = next(outcomes)
            if isinstance(result, Exception):
                raise result
            return result
        return smoke.SmokeRequestPolicy(clock=lambda: self.clock.now, sleep=self.clock.sleep,
                                        transport=transport, **kwargs)

    def limited(self, retry_after='60'):
        headers = Message()
        if retry_after is not None:
            headers['Retry-After'] = retry_after
        return HTTPError('https://cloud.test/v1/tugql', 429, 'limited', headers, io.BytesIO(b'quota exhausted'))

    def test_gateway_classes_and_actual_spacing(self):
        for path in ['/v1/tugql', '/v1/tugql/', '/v1/databases/a/tugql?q=x',
                     '/v1/databases/a/query', '/v1/databases/a/batch/', '/v1/databases/a/read',
                     '/v1/databases/a/tug%71l']:
            self.assertTrue(smoke.query_request(path, 'GET'), path)
            self.assertTrue(smoke.query_request(path, 'POST'), path)
            self.assertFalse(smoke.query_request(path, 'OPTIONS'), path)
        for path in ['/v1', '/ovdb/dbs/a', '/v1/databases/a', '/v1/databases/a/records/rows/1',
                     '/v1/databases/a/tugql/extra']:
            self.assertFalse(smoke.query_request(path, 'GET'), path)
        policy = self.policy([Response() for _ in range(5)])
        for path, method in [('/v1/tugql', 'GET'), ('/v1/databases/a/tugql', 'OPTIONS'),
                             ('/v1/databases/a/records/rows/1', 'GET'),
                             ('/v1/databases/a/read', 'POST'), ('/v1/databases/a/query', 'GET')]:
            policy.request('https://cloud.test', path, method)
        self.assertEqual([0, 0, 0, 1.1, 2.2], [at for _, at, _ in self.calls])

    def test_429_retries_identical_read_intent_retains_both_responses(self):
        policy = self.policy([self.limited(), Response(b'actual row')])
        status, _, body = policy.request('https://cloud.test', '/v1/databases/a/tugql', 'POST',
                                        b'{"query":"from rows"}', {'Origin': 'https://site.test'})
        self.assertEqual((200, b'actual row'), (status, body))
        self.assertEqual([60], self.clock.waits)
        first, second = [call[0] for call in self.calls]
        self.assertEqual((first.full_url, first.method, first.data, dict(first.headers)),
                         (second.full_url, second.method, second.data, dict(second.headers)))
        self.assertEqual([429, 200], [entry['status'] for entry in smoke.OBSERVATIONS])
        self.assertEqual('60', smoke.OBSERVATIONS[0]['headers']['Retry-After'])
        self.assertEqual('cXVvdGEgZXhoYXVzdGVk', smoke.OBSERVATIONS[0]['bodyBase64'])
        self.assertEqual(64, len(smoke.OBSERVATIONS[1]['bodySha256']))

    def test_exhausted_retries_remain_failed_assertion(self):
        policy = self.policy([self.limited('1') for _ in range(3)])
        with patch.object(smoke, 'REQUEST_POLICY', policy):
            with self.assertRaisesRegex(RuntimeError, 'returned 429, expected 200'):
                smoke.require_status('https://cloud.test', '/v1/tugql')
        self.assertEqual(3, len(self.calls))
        self.assertEqual([429, 429, 429], [entry['status'] for entry in smoke.OBSERVATIONS])

    def test_invalid_retry_after_refuses_without_another_request(self):
        for value in [None, '', '-1', '1.5', 'NaN', 'Wed, 21 Oct 2026 07:28:00 GMT', '121', '999999999999']:
            with self.subTest(value=value):
                self.calls.clear()
                policy = self.policy([self.limited(value)])
                with self.assertRaisesRegex(RuntimeError, 'Retry-After'):
                    policy.request('https://cloud.test', '/v1/tugql')
                self.assertEqual(1, len(self.calls))

    def test_non_429_is_never_retried_and_readonly_probe_stays_403(self):
        for status in [401, 403, 404, 500, 503]:
            with self.subTest(status=status):
                self.calls.clear()
                error = HTTPError('https://cloud.test/v1/tugql', status, 'denied', Message(), io.BytesIO(b'denied'))
                policy = self.policy([error])
                with patch.object(smoke, 'REQUEST_POLICY', policy):
                    if status == 403:
                        smoke.require_status('https://cloud.test', '/v1/databases/a/records/x/1', expected=403, method='PUT')
                    else:
                        with self.assertRaisesRegex(RuntimeError, f'returned {status}'):
                            smoke.require_status('https://cloud.test', '/v1/tugql')
                self.assertEqual(1, len(self.calls))

    def test_parameters_are_finite_bounded_and_zero_retries_does_not_wait(self):
        for kwargs in [dict(query_interval=float('nan')), dict(query_interval=float('inf')),
                       dict(query_interval=-1), dict(query_interval=61), dict(retries=-1),
                       dict(retries=6), dict(retries=1.1), dict(max_retry_after=float('nan')),
                       dict(max_retry_after=float('inf')), dict(max_retry_after=-1), dict(max_retry_after=301)]:
            with self.subTest(kwargs=kwargs), self.assertRaises(ValueError):
                self.policy(**kwargs)
        policy = self.policy([self.limited()], retries=0)
        self.assertEqual(429, policy.request('https://cloud.test', '/v1/tugql')[0])
        self.assertEqual([], self.clock.waits)
        fresh = self.policy([Response()])
        self.assertEqual(None, fresh.last_query)
        self.assertEqual({'queryIntervalSeconds': 1.1, 'rateLimitRetries': 2,
                          'maxRetryAfterSeconds': 120}, fresh.settings())

    def test_zero_retry_after_still_obeys_query_spacing(self):
        policy = self.policy([self.limited('0'), Response()])
        self.assertEqual(200, policy.request('https://cloud.test', '/v1/tugql')[0])
        self.assertEqual([0, 1.1], [at for _, at, _ in self.calls])
        self.assertEqual([429, 200], [entry['status'] for entry in smoke.OBSERVATIONS])

    def test_each_cli_invocation_resets_pending_policy_and_observations(self):
        with tempfile.TemporaryDirectory() as directory:
            inventory = Path(directory) / 'inventory.json'
            inventory.write_text(json.dumps({'version': 2, 'databases': []}))
            for interval in ['0.5', '1.1']:
                smoke.REQUEST_POLICY.last_query = 999
                smoke.OBSERVATIONS.append({'status': 429})
                with patch('sys.argv', ['smoke_inventory.py', '--origin', 'https://cloud.test',
                            '--inventory', str(inventory), '--query-interval-seconds', interval]):
                    with self.assertRaisesRegex(RuntimeError, 'at least one database'):
                        smoke.main()
                self.assertIsNone(smoke.REQUEST_POLICY.last_query)
                self.assertEqual(float(interval), smoke.REQUEST_POLICY.query_interval)
                self.assertEqual([], smoke.OBSERVATIONS)

    def test_cli_validates_before_inventory_or_transport(self):
        for arguments in [['--query-interval-seconds', 'nan'], ['--rate-limit-retries', '6'],
                          ['--max-retry-after-seconds', 'inf']]:
            with self.subTest(arguments=arguments), patch('sys.argv', ['smoke_inventory.py', '--origin',
                    'https://cloud.test', '--inventory', '/does-not-exist', *arguments]), \
                    patch('sys.stderr', io.StringIO()), patch.object(smoke, 'urlopen') as transport:
                with self.assertRaises(SystemExit) as error:
                    smoke.main()
                self.assertEqual(2, error.exception.code)
                transport.assert_not_called()


if __name__ == '__main__':
    unittest.main()
