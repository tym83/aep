import { Link, useSidebarOpenState } from '@backstage/core-components';
import { configApiRef, useApi } from '@backstage/core-plugin-api';
export function SidebarLogo() {
  const config = useApi(configApiRef);
  const { isOpen } = useSidebarOpenState();
  return (
    <Link to="/" aria-label={config.getString('app.branding.name')}>
      <img
        style={{ margin: 12, maxWidth: isOpen ? 180 : 24, height: 32 }}
        src={config.getString(
          `app.branding.${isOpen ? 'fullLogo' : 'iconLogo'}`,
        )}
        alt={config.getString('app.branding.name')}
      />
    </Link>
  );
}
