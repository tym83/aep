import { render, screen } from '@testing-library/react';
import App from './App';

it('assembles NFS features and offers only the configured OIDC sign-in', async () => {
  process.env = {
    ...process.env,
    APP_CONFIG: [
      {
        data: {
          app: { title: 'AEP Test', branding: { name: 'AEP Test' } },
          backend: { baseUrl: 'http://localhost:7007' },
          auth: { environment: 'development' },
          openchoreo: { features: { auth: { enabled: true } } },
        },
        context: 'test',
      },
    ] as any,
  };
  render(App.createRoot());
  expect(
    await screen.findByText('Sign in with your organization account'),
  ).toBeInTheDocument();
  expect(screen.getAllByText('AEP Test').length).toBeGreaterThan(0);
  expect(screen.queryByText(/guest/i)).not.toBeInTheDocument();
});
