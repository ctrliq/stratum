import { createFileRoute } from '@tanstack/react-router';

import { FleetScreen } from '~/domain/clusters/components/FleetScreen/FleetScreen';
import {
  PageLoading,
  PageUnloaded,
} from '~/domain/operations/components/PageUnloaded/PageUnloaded';
import { OperationsList } from '~/domain/operations/pages/OperationsList';

function OperationsRoute() {
  return (
    <FleetScreen screenId="operations">
      <OperationsList />
    </FleetScreen>
  );
}

export const Route = createFileRoute('/operations')({
  component: OperationsRoute,
  pendingComponent: PageLoading,
  errorComponent: PageUnloaded,
  pendingMs: 0,
  codeSplitGroupings: [['component']],
});
