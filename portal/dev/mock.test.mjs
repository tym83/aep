import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mockServer } from './mock.mjs';
test('mock preserves organization isolation, idempotency and decision preconditions', async () => {
  const server = mockServer();
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const url = `http://127.0.0.1:${server.address().port}`;
  const call = (path, token, method = 'GET', body, headers = {}) =>
    fetch(url + path, {
      method,
      headers: {
        Authorization: `Bearer ${token}`,
        'Content-Type': 'application/json',
        ...headers,
      },
      body: body ? JSON.stringify(body) : undefined,
    });
  try {
    assert.equal((await call('/v1/me', 'invalid')).status, 401);
    assert.equal(
      (await call('/v1/organizations/other/services', 'demo-requester')).status,
      404,
    );
    const input = {
      organization: 'acme',
      operation: 'promote',
      target: { kind: 'Component', name: 'api' },
      desired: { releaseName: 'v1' },
      justification: 'Release',
    };
    const created = await (
      await call('/v1/changes', 'demo-requester', 'POST', input, {
        'Idempotency-Key': 'k',
      })
    ).json();
    assert.equal(
      (
        await (
          await call('/v1/changes', 'demo-requester', 'POST', input, {
            'Idempotency-Key': 'k',
          })
        ).json()
      ).id,
      created.id,
    );
    assert.equal(
      (
        await call(
          '/v1/changes',
          'demo-requester',
          'POST',
          { ...input, justification: 'Other' },
          { 'Idempotency-Key': 'k' },
        )
      ).status,
      409,
    );
    const path = `/v1/changes/${created.id}/decision`;
    assert.equal(
      (
        await call(
          path,
          'demo-requester',
          'POST',
          { decision: 'approve' },
          { 'If-Match': '1' },
        )
      ).status,
      403,
    );
    assert.equal(
      (
        await call(
          path,
          'demo-approver',
          'POST',
          { decision: 'approve' },
          { 'If-Match': '0' },
        )
      ).status,
      412,
    );
    assert.equal(
      (
        await call(
          path,
          'demo-approver',
          'POST',
          { decision: 'approve' },
          { 'If-Match': '1' },
        )
      ).status,
      200,
    );
    assert.equal(
      (
        await call(
          path,
          'demo-approver',
          'POST',
          { decision: 'reject' },
          { 'If-Match': '1' },
        )
      ).status,
      412,
    );
  } finally {
    await new Promise(resolve => server.close(resolve));
  }
});
