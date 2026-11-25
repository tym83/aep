import {
  createApiRef,
  DiscoveryApi,
  FetchApi,
} from '@backstage/core-plugin-api';
export type Session = {
  username: string;
  organizations: { name: string; roles: string[] }[];
};
export type Service = {
  name: string;
  kind: 'cluster' | 'service';
  labels: Record<string, string>;
  application: { name: string; namespace: string; consoleUrl: string };
  claim: {
    name: string;
    generation: number;
    conditions: {
      type: string;
      status: string;
      reason?: string;
      message?: string;
      observedGeneration?: number;
    }[];
  };
};
export type Change = {
  metadata: { name: string; resourceVersion: string };
  spec: {
    organization: string;
    operation: string;
    requester: string;
    justification: string;
    target: {
      kind: string;
      name: string;
      project?: string;
      environment?: string;
    };
    window?: { start: string; end: string };
  };
  status?: {
    phase?: string;
    approvals?: {
      user: string;
      decision: string;
      role: string;
      time: string;
    }[];
  };
};
export type ChangeInput = {
  organization: string;
  operation: string;
  model: string;
  target: Change['spec']['target'];
  desired: Record<string, unknown>;
  justification: string;
};
export const aepApiRef = createApiRef<AepApi>({ id: 'plugin.aep.api' });
export class AepApi {
  constructor(
    private discovery: DiscoveryApi,
    private fetchApi: FetchApi,
    private auth: { getAccessToken(): Promise<string> },
  ) {}
  async request<T>(path: string, options?: RequestInit): Promise<T> {
    const headers = new Headers(options?.headers);
    headers.set('x-openchoreo-token', await this.auth.getAccessToken());
    const response = await this.fetchApi.fetch(
      `${await this.discovery.getBaseUrl('aep')}${path}`,
      { ...options, headers },
    );
    if (!response.ok)
      throw new Error(
        `AEP request failed (${response.status}): ${await response.text()}`,
      );
    return response.json();
  }
  session() {
    return this.request<Session>('/v1/me');
  }
  services(org: string) {
    return this.request<{ items: Service[] }>(
      `/v1/organizations/${encodeURIComponent(org)}/services`,
    );
  }
  changes(org: string) {
    return this.request<{ items: Change[] }>(
      `/v1/organizations/${encodeURIComponent(org)}/changes`,
    );
  }
  change(org: string, name: string) {
    return this.request<{ request: Change }>(
      `/v1/changes/${encodeURIComponent(org)}/${encodeURIComponent(name)}`,
    );
  }
  create(input: ChangeInput, key: string) {
    return this.request<{ id: string; request: Change }>('/v1/changes', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Idempotency-Key': key },
      body: JSON.stringify(input),
    });
  }
  decide(change: Change, decision: string) {
    return this.request<{ request: Change }>(
      `/v1/changes/${encodeURIComponent(
        change.spec.organization,
      )}/${encodeURIComponent(change.metadata.name)}/decision`,
      {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'If-Match': change.metadata.resourceVersion,
        },
        body: JSON.stringify({ decision }),
      },
    );
  }
}
export function serviceState(service: Service) {
  const conditions = service.claim.conditions;
  const current = conditions.filter(
    c => c.observedGeneration === service.claim.generation,
  );
  const degraded = current.find(
    c => c.type === 'Degraded' && c.status === 'True',
  );
  const ready = current.find(c => c.type === 'Ready');
  const condition = degraded ?? ready;
  return {
    phase: degraded
      ? 'Degraded'
      : ready?.status === 'True'
      ? 'Ready'
      : 'Updating',
    reason:
      condition?.message ||
      condition?.reason ||
      'Waiting for current ServiceClaim status',
  };
}
