type PayloadType = {
  method: string
  param: Array<Object>
  response?: Object
}

export type SendArg = [number, number, unknown, unknown]

// Client.invoke in client.iced in framed-msgpack-rpc ostensibly takes
// a list of arguments, but it expects exactly one element with keyed
// arguments.
export type InvokeType = (method: string, args: [Object], cb: (err: any, data: any) => void) => void
export type CreateClientType = {
  transport: {
    needsConnect: boolean
    reset: () => void
    send: (buf: Uint8Array) => void
  }
  invoke: InvokeType
}

export type IncomingRPCCallbackType = (payload: PayloadType) => void
export type RpcLogType = 'engineToServer' | 'serverToEngine' | 'engineInternal'
export type ConnectDisconnectCB = () => void

declare function createClient(
  incomingRPCCallback: IncomingRPCCallbackType,
  connectCallback: ConnectDisconnectCB,
  disconnectCallback: ConnectDisconnectCB
): CreateClientType

declare function resetClient(client: CreateClientType): void

declare function rpcLog(arg0: {method: string; reason?: string; extra?: Object; type: string}): void

export {createClient, resetClient, rpcLog}
