import { RuntimeDataSource, registerRuntimeDataSource } from '@grafana/scenes'
import { DataQueryRequest, DataQueryResponse, FieldType, createDataFrame, LoadingState } from '@grafana/data'

export interface JaegerQuery {
  refId: string
  service?: string
}

const SKIP_OPS = new Set(['/health', 'health_check', '/metrics', 'grpc.health.v1.Health/Check'])

export class JaegerDataSource extends RuntimeDataSource<JaegerQuery> {
  static readonly UID = 'jaeger-rt'

  constructor() {
    super('jaeger-plugin', JaegerDataSource.UID)
  }

  query(req: DataQueryRequest<JaegerQuery>): Promise<DataQueryResponse> {
    const target = req.targets[0]
    return this.fetch(target?.service ?? 'gateway-service')
  }

  private async fetch(service: string): Promise<DataQueryResponse> {
    const qs = new URLSearchParams({ service, limit: '50' })
    const res = await fetch(`/jaeger/api/traces?${qs}`)
    if (!res.ok) throw new Error(`Jaeger ${res.status}`)

    const { data: traces = [] } = (await res.json()) as { data: JaegerTrace[] }

    const traceIds: string[] = []
    const services: string[] = []
    const operations: string[] = []
    const statuses: string[] = []
    const durations: number[] = []
    const times: string[] = []

    for (const trace of traces) {
      for (const span of trace.spans) {
        if (SKIP_OPS.has(span.operationName)) continue

        const tags: Record<string, string> = {}
        for (const t of span.tags) tags[t.key] = String(t.value)

        // Skip low-level infra spans for non-technical readers
        if (tags['db.system'] || tags['messaging.system']) continue
        if (tags['span.kind'] === 'internal' && !tags['http.method']) continue

        const svc = trace.processes[span.processID]?.serviceName ?? ''
        const code = Number(tags['http.status_code'])
        const isErr = tags['error'] === 'true' || code >= 400
        const status = isErr ? `Error${code ? ` (${code})` : ''}` : code ? `HTTP ${code}` : 'OK'

        traceIds.push(trace.traceID.slice(0, 8))
        services.push(svc)
        operations.push(span.operationName)
        statuses.push(status)
        durations.push(Math.round(span.duration / 1000))
        times.push(new Date(span.startTime / 1000).toLocaleTimeString())
      }
    }

    return {
      state: LoadingState.Done,
      data: [
        createDataFrame({
          refId: 'A',
          fields: [
            { name: 'Trace', type: FieldType.string, values: traceIds },
            { name: 'Service', type: FieldType.string, values: services },
            { name: 'Operation', type: FieldType.string, values: operations },
            { name: 'Status', type: FieldType.string, values: statuses },
            { name: 'Duration (ms)', type: FieldType.number, values: durations },
            { name: 'Time', type: FieldType.string, values: times },
          ],
        }),
      ],
    }
  }
}

interface JaegerTrace {
  traceID: string
  spans: JaegerSpan[]
  processes: Record<string, { serviceName: string }>
}

interface JaegerSpan {
  spanID: string
  operationName: string
  processID: string
  duration: number
  startTime: number
  tags: Array<{ key: string; value: unknown }>
}

let registered = false
export function setupJaeger() {
  if (!registered) {
    registerRuntimeDataSource({ dataSource: new JaegerDataSource() })
    registered = true
  }
}
