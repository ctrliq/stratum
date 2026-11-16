import { useQueryClient } from '@tanstack/react-query';
import { useRouter } from '@tanstack/react-router';
import { useContext } from 'react';

import { listCoreV1NodeQuery, usePatchCoreV1Node } from '~/api/k8s/k8sComponents';
import { fromPathname } from '~/domain/shell/utils/parsers';
import { toast } from '~/shared/components/Toaster/toast';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { toErrorNotice } from '~/shared/utils/formatters';

import type { NodeRow } from '../types/nodes';
import { toCordonNotice } from '../utils/formatters';

type Cordonable = Pick<NodeRow, 'name' | 'isCordoned'>;

export const useCordon = () => {
  const clusterId = useContext(ClusterContext);
  const queryClient = useQueryClient();
  const router = useRouter();
  const { mutateAsync: patched, isPending } = usePatchCoreV1Node();
  const isStillShown = (): boolean =>
    fromPathname(router.state.location.pathname).clusterId === clusterId;

  const onCordonToggled = ({ name, isCordoned }: Cordonable): void => {
    void patched({ pathParams: { name }, body: { spec: { unschedulable: !isCordoned } } }).then(
      () => {
        if (isStillShown()) {
          toast(toCordonNotice(name, !isCordoned));
          void queryClient.invalidateQueries({
            queryKey: listCoreV1NodeQuery({ clusterId }).queryKey,
          });
        }
      },
      (error: unknown) => {
        if (isStillShown()) {
          toast.error(toErrorNotice(error));
        }
      },
    );
  };

  return { onCordonToggled, isCordoning: isPending };
};
