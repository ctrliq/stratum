import { createFileRoute } from '@tanstack/react-router';

import { HubOnly } from '~/domain/hardening/components/HubOnly/HubOnly';
import { HardeningPosture } from '~/domain/hardening/pages/HardeningPosture';

function HardeningRoute() {
  return (
    <HubOnly>
      <HardeningPosture />
    </HubOnly>
  );
}

export const Route = createFileRoute('/c/$cluster/hardening')({
  component: HardeningRoute,
});
