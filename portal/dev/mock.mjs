import { createServer } from 'node:http';
import { pathToFileURL } from 'node:url';
// Development only. Opaque demo tokens are never accepted by the production plugin.
export function mockServer() {
  const changes = new Map();
  const keys = new Map();
  let sequence = 0;
  const service = {
    name: 'orders-db-prod',
    kind: 'service',
    labels: {
      'aep.aenix.io/project': 'payments',
      'aep.aenix.io/resource': 'orders-db',
      'aep.aenix.io/environment': 'prod',
    },
    application: {
      name: 'postgres-orders',
      namespace: 'tenant-acme-prod',
      consoleUrl:
        'https://console.example.test/tenant-acme-prod/postgres-orders',
    },
    claim: {
      name: 'orders-db',
      generation: 1,
      conditions: [
        {
          type: 'Ready',
          status: 'True',
          observedGeneration: 1,
          reason: 'Reconciled',
        },
      ],
    },
  };
  return createServer(async (req, res) => {
    const token = req.headers.authorization?.replace(/^Bearer /, '');
    const username =
      token === 'demo-requester'
        ? 'alice'
        : token === 'demo-approver'
        ? 'bob'
        : undefined;
    const send = (code, body) => {
      res.writeHead(code, { 'content-type': 'application/json' });
      res.end(JSON.stringify(body));
    };
    if (!username)
      return send(401, { error: 'Use demo-requester or demo-approver' });
    const path = new URL(req.url, 'http://localhost').pathname;
    if (req.method === 'GET' && path === '/v1/me')
      return send(200, {
        username,
        organizations: [
          {
            name: 'acme',
            roles: username === 'bob' ? ['approver'] : ['developer'],
          },
        ],
      });
    if (req.method === 'GET' && path === '/v1/organizations/acme/services')
      return send(200, {
        items: [
          service,
          {
            ...service,
            name: 'prod-cluster',
            kind: 'cluster',
            labels: {
              ...service.labels,
              'aep.aenix.io/resource': 'Production cluster',
            },
          },
        ],
      });
    if (req.method === 'GET' && path === '/v1/organizations/acme/changes')
      return send(
        username === 'bob' ? 200 : 403,
        username === 'bob'
          ? { items: [...changes.values()] }
          : { error: 'Approver required' },
      );
    let body = '';
    for await (const chunk of req) {
      body += chunk;
      if (body.length > 65536) return send(413, { error: 'Body too large' });
    }
    let input;
    try {
      input = body ? JSON.parse(body) : {};
    } catch {
      return send(400, { error: 'Invalid JSON' });
    }
    if (req.method === 'POST' && path === '/v1/changes') {
      if (input.organization !== 'acme')
        return send(403, { error: 'Organization denied' });
      const key = req.headers['idempotency-key'];
      if (!key) return send(400, { error: 'Idempotency-Key required' });
      const scope = `${username}:${key}`;
      const previous = keys.get(scope);
      if (previous)
        return send(
          previous.body === body ? 200 : 409,
          previous.body === body
            ? previous.view
            : { error: 'Key reused for different input' },
        );
      const name = `chg-${++sequence}`;
      const change = {
        metadata: { name, resourceVersion: '1' },
        spec: { ...input, requester: username },
        status: { phase: 'Pending', approvals: [] },
      };
      changes.set(name, change);
      const view = { id: `acme/${name}`, request: change };
      keys.set(scope, { body, view });
      return send(201, view);
    }
    const match = path.match(
      /^\/v1\/changes\/acme\/([a-z0-9-]+)(\/decision)?$/,
    );
    if (match) {
      const change = changes.get(match[1]);
      if (!change) return send(404, { error: 'Not found' });
      if (req.method === 'GET' && !match[2])
        return send(200, { id: `acme/${match[1]}`, request: change });
      if (req.method === 'POST' && match[2]) {
        if (username !== 'bob' || change.spec.requester === username)
          return send(403, { error: 'Independent approver required' });
        if (!req.headers['if-match'])
          return send(428, { error: 'If-Match required' });
        if (req.headers['if-match'] !== change.metadata.resourceVersion)
          return send(412, { error: 'Stale version' });
        if (change.status.phase !== 'Pending')
          return send(409, { error: 'Not pending' });
        if (!['approve', 'reject'].includes(input.decision))
          return send(400, { error: 'Invalid decision' });
        change.metadata.resourceVersion = String(
          Number(change.metadata.resourceVersion) + 1,
        );
        change.status.phase =
          input.decision === 'approve' ? 'Approved' : 'Rejected';
        change.status.approvals.push({
          user: username,
          role: 'approver',
          decision: input.decision,
          time: new Date().toISOString(),
        });
        return send(200, { id: `acme/${match[1]}`, request: change });
      }
    }
    return send(404, { error: 'Not found or organization denied' });
  });
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href)
  mockServer().listen(7100, '127.0.0.1', () =>
    process.stdout.write('AEP mock listening on 127.0.0.1:7100\n'),
  );
