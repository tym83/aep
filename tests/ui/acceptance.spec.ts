import { test, expect, Page } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';

function required(key: string): string {
  const value = process.env[key];
  if (!value) throw new Error(`Required environment variable: ${key}`);
  return value;
}
const selectors: Record<string, string> = JSON.parse(process.env.AEP_SELECTORS ?? '{}');
function selector(key: string, fallback: string): string { return selectors[key] ?? fallback; }
async function login(page: Page, role: string) {
  await page.goto('/');
  const popupPromise = page.waitForEvent('popup');
  await page.locator(selector('signIn', 'button:has-text("Sign In")')).click();
  const idp = await popupPromise;
  await idp.locator(selector('username', '#username')).fill(required(`AEP_${role}_USER`));
  await idp.locator(selector('password', '#password')).fill(required(`AEP_${role}_PASSWORD`));
  await Promise.all([idp.waitForEvent('close'), idp.locator(selector('submit', '#kc-login')).click()]);
  await expect(page.locator(selector('signedIn', 'nav'))).toBeVisible();
}

test('developer catalog, infrastructure card and organization isolation', async ({ page }) => {
  await login(page, 'DEVELOPER');
  await page.goto(process.env.AEP_CATALOG_PATH ?? '/catalog');
  await expect(page.getByText(required('AEP_OWN_ENTITY'), { exact: true })).toBeVisible();
  await expect(page.getByText(required('AEP_OTHER_ENTITY'), { exact: true })).toHaveCount(0);
  await page.goto(required('AEP_OWN_ENTITY_PATH'));
  await expect(page.locator(selector('infrastructure', 'text=AEP Infrastructure'))).toBeVisible();
  // Direct navigation also must not reveal an entity hidden by catalog filtering.
  await page.goto(required('AEP_OTHER_ENTITY_PATH'));
  await expect(page.locator(selector('entityDenied', 'text=/not found|access denied|forbidden/i'))).toBeVisible();
  await expect(page.getByText(required('AEP_OTHER_ENTITY'), { exact: true })).toHaveCount(0);
});

test('approver sees a pending change by another person and approves it', async ({ page, request }) => {
  const cmd: string[] = JSON.parse(required('AEP_TOKEN_COMMAND'));
  const developer = required('AEP_DEVELOPER_USER');
  expect(required('AEP_APPROVER_USER')).not.toBe(developer);
  let token: string;
  try {
    token = execFileSync(cmd[0], [...cmd.slice(1), developer], { encoding: 'utf8', timeout: 30_000, stdio: ['ignore', 'pipe', 'pipe'] }).trim();
  } catch { throw new Error('Token provider failed (output redacted)'); }
  if (!token || /[\r\n]/.test(token)) throw new Error('Invalid token provider output');
  const base = required('AEP_CHANGE_URL').replace(/\/$/, '');
  const org = required('AEP_ORGANIZATION');
  const run = `ui-${randomUUID().slice(0, 18)}`;
  // Approved decommission of an absent, unique target exercises approvals
  // without provisioning production data. Its audit record is retained.
  const response = await request.post(`${base}/v1/changes`, {
    headers: { Authorization: `Bearer ${token}`, 'Idempotency-Key': run },
    data: { organization: org, operation: 'decommission', model: 'normal',
      target: { kind: 'Resource', name: run, project: required('AEP_PROJECT'), environment: required('AEP_PROD_ENVIRONMENT') },
      desired: {}, justification: `Browser acceptance ${run}` },
  });
  expect(response.status()).toBe(201);
  const created = await response.json() as { request: { metadata: { name: string } } };
  const id = created.request.metadata.name;
  async function phase() {
    const r = await request.get(`${base}/v1/changes/${org}/${id}`, { headers: { Authorization: `Bearer ${token}` } });
    expect(r.status()).toBe(200);
    return (await r.json() as { request: { status?: { phase?: string } } }).request.status?.phase;
  }
  await expect.poll(phase, { timeout: 90_000 }).toBe('Pending');
  await login(page, 'APPROVER');
  await page.goto(process.env.AEP_CHANGES_PATH ?? '/aep/changes');
  const row = page.locator(selector('changeRow', 'li')).filter({ hasText: id });
  await expect(row).toContainText('Pending');
  await row.getByRole('button', { name: id, exact: true }).click();
  await page.locator(selector('approve', 'button:has-text("Approve")')).click();
  await expect.poll(phase, { timeout: 90_000 }).toMatch(/Approved|Applying|Applied/);
});

test('auditor has read access and no create controls', async ({ page }) => {
  await login(page, 'AUDITOR');
  for (const path of [process.env.AEP_CATALOG_PATH ?? '/catalog', '/aep/compliance', '/aep/costs']) {
    await page.goto(path);
    await expect(page.locator(selector('pageReady', 'main'))).toBeVisible();
    await expect(page.locator(selector('createButtons', 'button:has-text("Create"), a:has-text("Create"), button:has-text("Request change")'))).toHaveCount(0);
  }
});
