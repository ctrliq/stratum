export type ServiceCells = {
  type: string;
  clusterIp: string;
  ports: string;
};

export type EndpointsCells = {
  addresses: string;
};

export type EndpointSliceCells = {
  service: string;
  addressType: string;
  endpoints: number;
  ports: string;
};

export type IngressCells = {
  ingressClass: string;
  hosts: string;
};

export type IngressClassCells = {
  controller: string;
};

export type NetworkPolicyCells = {
  policyTypes: string;
};
