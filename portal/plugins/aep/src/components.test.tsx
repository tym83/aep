import '@testing-library/jest-dom';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { TestApiProvider, renderInTestApp } from '@backstage/test-utils';
import { aepApiRef, Service, serviceState } from './api';
import {
  RequestChange,
  ServiceSummary,
  ChangesPage,
  CostPage,
  CostTable,
} from './components';
const service: Service = {
  name: 'generated-name',
  kind: 'service',
  labels: {
    'aep.aenix.io/resource': 'orders-db',
    'aep.aenix.io/project': 'payments',
    'aep.aenix.io/environment': 'prod',
  },
  application: {
    name: 'pg-1',
    namespace: 'tenant-acme',
    consoleUrl: 'https://console.example.test/pg-1',
  },
  claim: {
    name: 'claim',
    generation: 2,
    conditions: [
      {
        type: 'Ready',
        status: 'True',
        observedGeneration: 2,
        reason: 'Reconciled',
      },
    ],
  },
};
it('renders bridge names, current claim state and console link', () => {
  render(<ServiceSummary service={service} />);
  expect(screen.getByText('orders-db · payments · prod')).toBeInTheDocument();
  expect(screen.getByText(/Ready: Reconciled/)).toBeInTheDocument();
  expect(screen.getByRole('link')).toHaveAttribute(
    'href',
    service.application.consoleUrl,
  );
  expect(
    serviceState({ ...service, claim: { ...service.claim, generation: 3 } })
      .phase,
  ).toBe('Updating');
  expect(
    serviceState({
      ...service,
      claim: {
        ...service.claim,
        conditions: [
          {
            type: 'Degraded',
            status: 'True',
            observedGeneration: 2,
            reason: 'Failed',
          },
        ],
      },
    }).phase,
  ).toBe('Degraded');
});
it('requests a change and retries with the same idempotency key after a lost response', async () => {
  Object.defineProperty(globalThis.crypto, 'randomUUID', {
    configurable: true,
    value: () => 'request-key',
  });
  const create = jest
    .fn()
    .mockRejectedValueOnce(new Error('Network lost'))
    .mockResolvedValue({ id: 'acme/chg-1' });
  await renderInTestApp(
    <TestApiProvider apis={[[aepApiRef, { create }]]}>
      <RequestChange
        organization="acme"
        target={{
          kind: 'Resource',
          name: 'orders-db',
          project: 'payments',
          environment: 'prod',
        }}
        operation="credential-rotate"
      />
    </TestApiProvider>,
  );
  fireEvent.click(screen.getByRole('button', { name: 'Rotate password' }));
  fireEvent.change(screen.getByLabelText(/Justification/), {
    target: { value: 'Scheduled rotation' },
  });
  fireEvent.click(screen.getByRole('button', { name: 'Submit request' }));
  expect(await screen.findByRole('alert')).toHaveTextContent('Network lost');
  fireEvent.click(screen.getByRole('button', { name: 'Submit request' }));
  expect(await screen.findByRole('status')).toHaveTextContent('acme/chg-1');
  expect(create).toHaveBeenCalledTimes(2);
  expect(create.mock.calls[0]).toEqual(create.mock.calls[1]);
  expect(create.mock.calls[0][0]).toMatchObject({
    organization: 'acme',
    operation: 'credential-rotate',
    model: 'normal',
    justification: 'Scheduled rotation',
  });
});
it('does not fetch changes or expose decisions to a non-approver', async () => {
  const changes = jest.fn();
  await renderInTestApp(
    <TestApiProvider
      apis={[
        [
          aepApiRef,
          {
            session: async () => ({
              username: 'alice',
              organizations: [{ name: 'acme', roles: ['developer'] }],
            }),
            changes,
          },
        ],
      ]}
    >
      <ChangesPage />
    </TestApiProvider>,
  );
  await waitFor(() =>
    expect(
      screen.getByText('The approver role is required.'),
    ).toBeInTheDocument(),
  );
  expect(changes).not.toHaveBeenCalled();
  expect(
    screen.queryByRole('button', { name: 'Approve' }),
  ).not.toBeInTheDocument();
});
it('loads detail and clears stale decisions so the approver must re-read', async () => {
  const change = {
    metadata: { name: 'chg-1', resourceVersion: '5' },
    spec: {
      organization: 'acme',
      requester: 'alice',
      operation: 'promote',
      justification: 'Release',
      target: { kind: 'Component', name: 'api' },
    },
    status: { phase: 'Pending', approvals: [] },
  };
  const decide = jest.fn().mockRejectedValue(new Error('412 stale'));
  const api = {
    session: async () => ({
      username: 'bob',
      organizations: [{ name: 'acme', roles: ['approver'] }],
    }),
    changes: async () => ({ items: [change] }),
    change: async () => ({ request: change }),
    decide,
  };
  await renderInTestApp(
    <TestApiProvider apis={[[aepApiRef, api]]}>
      <ChangesPage />
    </TestApiProvider>,
  );
  fireEvent.click(await screen.findByRole('button', { name: 'chg-1' }));
  fireEvent.click(await screen.findByRole('button', { name: 'Approve' }));
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Reload the change',
  );
  expect(decide).toHaveBeenCalledWith(change, 'approve');
  expect(
    screen.queryByRole('button', { name: 'Approve' }),
  ).not.toBeInTheDocument();
});

it('matches a Resource binding and exposes only change-request actions', async () => {
  const { EntityProvider } = await import('@backstage/plugin-catalog-react');
  const { ResourceCard } = await import('./components');
  const api = {
    session: async () => ({
      username: 'alice',
      organizations: [{ name: 'acme', roles: ['developer'] }],
    }),
    services: jest.fn().mockResolvedValue({ items: [service] }),
  };
  await renderInTestApp(
    <TestApiProvider apis={[[aepApiRef, api]]}>
      <EntityProvider
        entity={{
          apiVersion: 'backstage.io/v1alpha1',
          kind: 'Resource',
          metadata: {
            name: 'catalog-generated-id',
            annotations: {
              'openchoreo.io/namespace': 'acme',
              'openchoreo.io/project': 'payments',
              'openchoreo.io/resource': 'orders-db',
            },
          },
        }}
      >
        <ResourceCard />
      </EntityProvider>
    </TestApiProvider>,
  );
  expect(
    await screen.findByRole('button', { name: 'Restore' }),
  ).toBeInTheDocument();
  expect(
    screen.getByRole('button', { name: 'Rotate password' }),
  ).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Resize' })).toBeInTheDocument();
  expect(api.services).toHaveBeenCalledWith('acme');
});

it('shows stale compliance as unknown and renders policy counts and failures', async () => {
  const { ComplianceSummary } = await import('./components');
  render(
    <ComplianceSummary
      tier={{
        tier: 'prod',
        stale: true,
        snapshot: {
          collectedAt: '2026-11-25T00:00:00Z',
          policies: {
            'aep-prod-images': { pass: 4, fail: 1, warn: 0, error: 0, skip: 0 },
          },
          failingResources: [
            {
              policy: 'aep-prod-images',
              rule: 'digest-only',
              apiVersion: 'v1',
              kind: 'Pod',
              namespace: 'dp-prod',
              name: 'orders',
            },
          ],
          failuresTruncated: true,
        },
      }}
    />,
  );
  expect(screen.getByRole('status')).toHaveTextContent('compliance is unknown');
  expect(screen.getByRole('table')).toHaveTextContent('aep-prod-images');
  expect(screen.getByText(/Pod dp-prod\/orders/)).toBeInTheDocument();
  expect(screen.getByText(/first 50 failures/)).toBeInTheDocument();
});

it('does not show a successful compliance result before the first collection', async () => {
  const { ComplianceSummary } = await import('./components');
  render(
    <ComplianceSummary
      tier={{ tier: 'nonprod', stale: true, snapshot: null }}
    />,
  );
  expect(
    screen.getByText('Waiting for the first collection.'),
  ).toBeInTheDocument();
  expect(screen.queryByRole('table')).not.toBeInTheDocument();
});

it('shows coverage and unallocated cost without hiding missing hours', () => {
  render(
    <CostTable
      items={[
        {
          day: '2026-11-24',
          group: 'prod',
          project: '',
          currency: 'USD',
          cost: 12.5,
          unallocatedCost: 2,
          sampledHours: 20,
        },
      ]}
    />,
  );
  expect(screen.getByText('12.50 USD')).toBeInTheDocument();
  expect(screen.getByText('2.00 USD')).toBeInTheDocument();
  expect(screen.getByText('20/24')).toBeInTheDocument();
});
it('loads organization costs and changes the grouping', async () => {
  const costs = jest.fn().mockResolvedValue({ items: [] });
  const session = jest
    .fn()
    .mockResolvedValue({
      username: 'finance',
      organizations: [{ name: 'acme', roles: ['finance'] }],
    });
  await renderInTestApp(
    <TestApiProvider apis={[[aepApiRef, { costs, session }]]}>
      <CostPage />
    </TestApiProvider>,
  );
  await waitFor(() =>
    expect(costs).toHaveBeenCalledWith(
      'acme',
      expect.any(String),
      expect.any(String),
      'project',
    ),
  );
  expect(
    await screen.findByText('No recorded costs for this period.'),
  ).toBeInTheDocument();
});
