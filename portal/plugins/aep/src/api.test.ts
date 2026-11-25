import { AepApi } from './api';
it('sends the exact optimistic version and creation key', async () => {
  const fetch = jest
    .fn()
    .mockResolvedValue({ ok: true, json: async () => ({}) });
  const api = new AepApi(
    { getBaseUrl: async () => 'http://backend/api/aep' },
    { fetch },
    { getAccessToken: async () => 'oidc-user-token' },
  );
  await api.create(
    {
      organization: 'acme',
      operation: 'promote',
      model: 'normal',
      target: { kind: 'Component', name: 'api' },
      desired: { releaseName: 'v1' },
      justification: 'Release',
    },
    'key-1',
  );
  expect(fetch.mock.calls[0][1].headers.get('x-openchoreo-token')).toBe(
    'oidc-user-token',
  );
  expect(fetch.mock.calls[0][1].headers.get('Idempotency-Key')).toBe('key-1');
  await api.decide(
    {
      metadata: { name: 'chg-1', resourceVersion: '42' },
      spec: {
        organization: 'acme',
        operation: 'promote',
        requester: 'alice',
        justification: 'Release',
        target: { kind: 'Component', name: 'api' },
      },
    },
    'reject',
  );
  expect(fetch.mock.calls[1][1].headers.get('If-Match')).toBe('42');
  expect(JSON.parse(fetch.mock.calls[1][1].body)).toEqual({
    decision: 'reject',
  });
});
