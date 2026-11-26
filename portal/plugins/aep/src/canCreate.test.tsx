import '@testing-library/jest-dom';
import { render, screen } from '@testing-library/react';
import { TestApiProvider } from '@backstage/test-utils';
import { aepApiRef } from './api';
import { useCanCreate } from './canCreate';

function Probe() {
  return <span>{useCanCreate() ? 'creator' : 'read-only'}</span>;
}
function session(roles: string[][], fail = false) {
  return {
    session: async () => {
      if (fail) throw new Error('unavailable');
      return {
        username: 'u',
        organizations: roles.map((r, i) => ({ name: `org${i}`, roles: r })),
      };
    },
  };
}

it.each([
  [[['developer']], 'creator'],
  [[['approver'], ['platform']], 'creator'],
  [[['admin']], 'creator'],
  [[['approver', 'auditor']], 'read-only'],
  [[['finance']], 'read-only'],
  [[], 'read-only'],
])('roles %j are %s', async (roles, expected) => {
  render(
    <TestApiProvider apis={[[aepApiRef, session(roles)]]}>
      <Probe />
    </TestApiProvider>,
  );
  expect(await screen.findByText(expected)).toBeInTheDocument();
});

it('treats an unavailable session as read-only', async () => {
  render(
    <TestApiProvider apis={[[aepApiRef, session([['developer']], true)]]}>
      <Probe />
    </TestApiProvider>,
  );
  expect(await screen.findByText('read-only')).toBeInTheDocument();
});
