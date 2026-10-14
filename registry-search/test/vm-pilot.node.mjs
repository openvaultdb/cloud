import assert from 'node:assert/strict';
import { test } from 'node:test';
import { verifyContainer } from '../vm-pilot.mjs';

function installedContainer() {
  return {
    Name: '/registry-search-typesense',
    Image: 'sha256:pinned-image',
    Config: {
      Image: 'typesense/typesense:30.2@sha256:610f2d34b1f93d00762869da2c67736775e5798d19a2c8b91b014b8a0cc1e110',
      Entrypoint: ['/opt/typesense-server'],
      Cmd: null,
      Labels: {
        'dev.datatug.owner': 'alex',
        'dev.datatug.task': '01a114c1-e447-7a81-a869-0d82ca2a7747',
        'dev.datatug.service': 'registry-search-vm-pilot',
      },
      Env: ['TYPESENSE_API_KEY=testkey', 'TYPESENSE_DATA_DIR=/data'],
    },
    HostConfig: {
      NetworkMode: 'bridge',
      Privileged: false,
      CapAdd: null,
      NanoCpus: 2_000_000_000,
      Memory: 3 * 1024 ** 3,
      MemorySwap: 3 * 1024 ** 3,
      RestartPolicy: { Name: 'unless-stopped' },
      LogConfig: { Type: 'json-file', Config: { 'max-size': '10m', 'max-file': '3' } },
      PortBindings: { '8108/tcp': [{ HostIp: '127.0.0.1', HostPort: '8108' }] },
    },
    Mounts: [{ Type: 'bind', Source: '/opt/datatug/registry-search/data', Destination: '/data', RW: true }],
  };
}

test('private pilot accepts its own Docker state and rejects public exposure or weaker limits', () => {
  const state = installedContainer();
  const image = { Id: 'sha256:pinned-image', Config: { Entrypoint: ['/opt/typesense-server'] } };
  const verify = () => verifyContainer(state, 'testkey', image);
  assert.doesNotThrow(verify);
  state.HostConfig.PortBindings['8108/tcp'][0].HostIp = '0.0.0.0';
  assert.throws(verify, /port binding/);
  state.HostConfig.PortBindings['8108/tcp'][0].HostIp = '127.0.0.1';
  state.HostConfig.NetworkMode = 'host';
  assert.throws(verify, /network or privilege/);
  state.HostConfig.NetworkMode = 'bridge';
  state.HostConfig.Privileged = true;
  assert.throws(verify, /network or privilege/);
  state.HostConfig.Privileged = false;
  state.HostConfig.CapAdd = ['NET_ADMIN'];
  assert.throws(verify, /network or privilege/);
  state.HostConfig.CapAdd = null;
  state.Config.Cmd = ['--api-address', '0.0.0.0'];
  assert.throws(verify, /image command/);
  state.Config.Cmd = null;
  state.HostConfig.MemorySwap = 0;
  assert.throws(verify, /CPU\/memory limits/);
  state.HostConfig.MemorySwap = 3 * 1024 ** 3;
  state.Config.Labels['dev.datatug.owner'] = 'unknown';
  assert.throws(verify, /owner label/);
});
