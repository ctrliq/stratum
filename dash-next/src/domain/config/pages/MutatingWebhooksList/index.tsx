import {
  useCreateAdmissionregistrationV1MutatingWebhookConfiguration,
  useDeleteAdmissionregistrationV1MutatingWebhookConfiguration,
  useListAdmissionregistrationV1MutatingWebhookConfiguration,
  useReplaceAdmissionregistrationV1MutatingWebhookConfiguration,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { mutatingWebhookKind, webhookColumns } from '../../constants';
import { toWebhookRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function MutatingWebhooksList({ linkedName }: Props) {
  const list = useListAdmissionregistrationV1MutatingWebhookConfiguration({}, resourceListQuery);
  const { mutateAsync: created } = useCreateAdmissionregistrationV1MutatingWebhookConfiguration();
  const { mutateAsync: replaced } = useReplaceAdmissionregistrationV1MutatingWebhookConfiguration();
  const { mutateAsync: deleted } = useDeleteAdmissionregistrationV1MutatingWebhookConfiguration();

  return (
    <ResourceBrowser
      kind={mutatingWebhookKind}
      list={list}
      columns={webhookColumns}
      toRow={toWebhookRow}
      linkedName={linkedName}
      writes={{
        onCreate: (_namespace, body) => created({ body }),
        onReplace: ({ name }, body) => replaced({ pathParams: { name }, body }),
        onDelete: ({ name }) => deleted({ pathParams: { name } }),
      }}
    />
  );
}
