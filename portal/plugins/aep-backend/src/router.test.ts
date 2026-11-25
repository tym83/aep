import express from 'express';
import request from 'supertest';
import { createRouter } from './router';

describe('AEP proxy', () => {
  const upstream = jest.fn();
  const app = express().use(
    createRouter({
      readApiUrl: 'http://read',
      changeApiUrl: 'http://change',
      fetch: upstream,
    }),
  );
  beforeEach(() => {
    upstream.mockReset();
    upstream.mockResolvedValue(
      new Response('{"items":[]}', { status: 200, headers: { etag: '7' } }),
    );
  });
  it('requires the user token and never substitutes the Backstage credential', async () => {
    expect(
      (
        await request(app)
          .get('/v1/me')
          .set('Authorization', 'Bearer backstage')
      ).status,
    ).toBe(401);
    expect(upstream).not.toHaveBeenCalled();
  });
  it('forwards only the identity provider credential to the read API', async () => {
    const result = await request(app)
      .get('/v1/organizations/acme/services')
      .set('x-openchoreo-token', 'user-token')
      .set('Authorization', 'Bearer backstage');
    expect(result.status).toBe(200);
    expect(result.headers.etag).toBe('7');
    expect(upstream).toHaveBeenCalledWith(
      'http://read/v1/organizations/acme/services',
      expect.objectContaining({
        headers: {
          Authorization: 'Bearer user-token',
          Accept: 'application/json',
        },
        redirect: 'error',
      }),
    );
  });
  it('preserves idempotency, optimistic locking and upstream authorization errors', async () => {
    expect(
      (
        await request(app)
          .post('/v1/changes')
          .set('x-openchoreo-token', 'token')
          .send({})
      ).status,
    ).toBe(400);
    await request(app)
      .post('/v1/changes')
      .set('x-openchoreo-token', 'token')
      .set('Idempotency-Key', 'retry-key')
      .send({ organization: 'acme' });
    expect(upstream).toHaveBeenLastCalledWith(
      'http://change/v1/changes',
      expect.objectContaining({
        headers: expect.objectContaining({ 'Idempotency-Key': 'retry-key' }),
        body: '{"organization":"acme"}',
      }),
    );
    expect(
      (
        await request(app)
          .post('/v1/changes/acme/chg-1/decision')
          .set('x-openchoreo-token', 'token')
          .send({})
      ).status,
    ).toBe(428);
    upstream.mockResolvedValue(
      new Response('{"error":"stale"}', { status: 412 }),
    );
    expect(
      (
        await request(app)
          .post('/v1/changes/acme/chg-1/decision')
          .set('x-openchoreo-token', 'token')
          .set('If-Match', '3')
          .send({ decision: 'approve' })
      ).status,
    ).toBe(412);
    expect(upstream).toHaveBeenLastCalledWith(
      'http://change/v1/changes/acme/chg-1/decision',
      expect.objectContaining({
        headers: expect.objectContaining({ 'If-Match': '3' }),
      }),
    );
    upstream.mockResolvedValue(new Response('{}', { status: 403 }));
    expect(
      (
        await request(app)
          .get('/v1/organizations/other/services')
          .set('x-openchoreo-token', 'token')
      ).status,
    ).toBe(403);
  });
  it('blocks arbitrary routes and methods; reports upstream failures without leaking tokens', async () => {
    expect((await request(app).delete('/v1/changes/acme/a')).status).toBe(404);
    expect((await request(app).get('/admin')).status).toBe(404);
    expect(upstream).not.toHaveBeenCalled();
    upstream.mockRejectedValue(new Error('secret-token'));
    const result = await request(app)
      .get('/v1/me')
      .set('x-openchoreo-token', 'secret-token');
    expect(result.status).toBe(502);
    expect(result.text).not.toContain('secret-token');
  });
});
