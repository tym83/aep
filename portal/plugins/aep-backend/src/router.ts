import express from 'express';

export function createRouter(options: {
  readApiUrl: string;
  changeApiUrl: string;
  fetch?: typeof fetch;
}) {
  const router = express.Router();
  const request = options.fetch ?? fetch;
  router.use(express.json({ limit: '64kb' }));
  // Only explicitly supported routes can reach upstream services. Never accept a URL from callers.
  router.use(async (req, res) => {
    const read =
      req.method === 'GET' &&
      (/^\/v1\/me$/.test(req.path) ||
        /^\/v1\/organizations\/[a-z0-9-]+\/(services|changes)$/.test(req.path));
    const change =
      (req.method === 'POST' && req.path === '/v1/changes') ||
      (req.method === 'GET' &&
        /^\/v1\/changes\/[a-z0-9-]+\/[a-z0-9-]+$/.test(req.path)) ||
      (req.method === 'POST' &&
        /^\/v1\/changes\/[a-z0-9-]+\/[a-z0-9-]+\/decision$/.test(req.path));
    if (!read && !change) {
      res.status(404).json({ error: 'Unsupported AEP route' });
      return;
    }
    const token = req.header('x-openchoreo-token');
    if (!token) {
      res.status(401).json({ error: 'User identity provider token required' });
      return;
    }
    const headers: Record<string, string> = {
      Authorization: `Bearer ${token}`,
      Accept: 'application/json',
    };
    if (req.method === 'POST') {
      headers['Content-Type'] = 'application/json';
      const required = req.path.endsWith('/decision')
        ? 'If-Match'
        : 'Idempotency-Key';
      const value = req.header(required);
      if (!value) {
        res
          .status(required === 'If-Match' ? 428 : 400)
          .json({ error: `${required} required` });
        return;
      }
      headers[required] = value;
    }
    try {
      const upstream = await request(
        `${(read ? options.readApiUrl : options.changeApiUrl).replace(
          /\/$/,
          '',
        )}${req.path}`,
        {
          method: req.method,
          headers,
          body: req.method === 'POST' ? JSON.stringify(req.body) : undefined,
          redirect: 'error',
          signal: AbortSignal.timeout(15000),
        },
      );
      const etag = upstream.headers.get('etag');
      if (etag) res.setHeader('ETag', etag);
      res
        .status(upstream.status)
        .type('application/json')
        .send(await upstream.text());
    } catch {
      res.status(502).json({ error: 'AEP upstream unavailable' });
    }
  });
  return router;
}
