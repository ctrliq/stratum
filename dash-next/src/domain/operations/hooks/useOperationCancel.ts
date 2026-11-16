import { useIsMutating, useQueryClient } from '@tanstack/react-query';

import { useCancelOperation } from '~/api/fleet/fleetComponents';
import type { Operation } from '~/api/fleet/fleetSchemas';
import { toast } from '~/shared/components/Toaster/toast';
import { toErrorMessage } from '~/shared/utils/formatters';

import { cancelMutationKey } from '../constants';
import { toCancelToast } from '../utils/formatters';
import { hasOperationId } from '../utils/predicates';
import { withCancelRequest } from '../utils/reducers';
import { readOperation, toOperationKey } from './useOperation';

export const useOperationCancel = (id: string) => {
  const queryClient = useQueryClient();
  const isBusy = useIsMutating({ mutationKey: cancelMutationKey }) > 0;
  const cancel = useCancelOperation({
    mutationKey: cancelMutationKey,
    onSuccess: (answer) => {
      queryClient.setQueryData<Operation>(toOperationKey(id), (shown) =>
        withCancelRequest(shown, answer),
      );
      return (
        hasOperationId(answer)
          ? readOperation(id).then(
              (fresh) => queryClient.setQueryData(toOperationKey(id), fresh),
              () => undefined,
            )
          : Promise.resolve(undefined)
      ).then(() => {
        toast(toCancelToast(id));
      });
    },
    onError: (error) => {
      toast.error(`Error: ${toErrorMessage(error)}`);
    },
  });
  return {
    isBusy,
    onCancelRequested: () => {
      cancel.mutate({ pathParams: { id } });
    },
  };
};
