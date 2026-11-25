import {
  createFrontendModule,
  configApiRef,
  useApi,
} from '@backstage/frontend-plugin-api';
import { SignInPageBlueprint } from '@backstage/plugin-app-react';
import { SignInPage } from '@backstage/core-components';
import { SignInPageProps } from '@backstage/core-plugin-api';
import { openChoreoAuthApiRef } from '@openchoreo/backstage-plugin';
function AepSignIn(props: SignInPageProps) {
  const config = useApi(configApiRef);
  return (
    <SignInPage
      {...props}
      provider={{
        id: 'openchoreo-auth',
        title: config.getString('app.branding.name'),
        message: 'Sign in with your organization account',
        apiRef: openChoreoAuthApiRef,
      }}
    />
  );
}
export const signInModule = createFrontendModule({
  pluginId: 'app',
  extensions: [
    SignInPageBlueprint.make({ params: { loader: async () => AepSignIn } }),
  ],
});
