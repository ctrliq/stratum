import {
  useCreateAdmissionregistrationV1ValidatingWebhookConfiguration,
  useDeleteAdmissionregistrationV1ValidatingWebhookConfiguration,
  useListAdmissionregistrationV1ValidatingWebhookConfiguration,
  useReplaceAdmissionregistrationV1ValidatingWebhookConfiguration,
} from '~/api/k8s/k8sComponents';
import { ResourceBrowser } from '~/domain/resources/components/ResourceBrowser';
import { resourceListQuery } from '~/domain/resources/constants';

import { validatingWebhookKind, webhookColumns } from '../../constants';
import { toWebhookRow } from '../../utils/transforms';

type Props = {
  linkedName?: string;
};

export function ValidatingWebhooksList({ linkedName }: Props) {
  const list = useListAdmissionregistrationV1ValidatingWebhookConfiguration({}, resourceListQuery);
  const { mutateAsync: created } = useCreateAdmissionregistrationV1ValidatingWebhookConfiguration();
  const { mutateAsync: replaced } =
    useReplaceAdmissionregistrationV1ValidatingWebhookConfiguration();
  const { mutateAsync: deleted } = useDeleteAdmissionregistrationV1ValidatingWebhookConfiguration();

  return (
    <ResourceBrowser
      kind={validatingWebhookKind}
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
