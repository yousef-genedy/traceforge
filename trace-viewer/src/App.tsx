import { useState } from 'react'
import { SceneContextProvider, useQueryRunner, TimeRangePicker } from '@grafana/scenes-react'
import { DataFrame, Field, LoadingState } from '@grafana/data'
import { JaegerDataSource } from './jaegerSource'

const SERVICES = ['gateway-service', 'order-service', 'user-service']

function field<T>(frame: DataFrame, name: string): T[] {
  return (frame.fields.find((f: Field) => f.name === name)?.values as T[]) ?? []
}

function TraceTable() {
  const [service, setService] = useState(SERVICES[0])
  const [filter, setFilter] = useState('')

  const runner = useQueryRunner({
    datasource: { uid: JaegerDataSource.UID, type: 'jaeger-plugin' },
    queries: [{ refId: 'A', service }],
    cacheKey: service,
  })

  const { data } = runner.useState()
  const isLoading = !data || data.state === LoadingState.Loading
  const frame = data?.series[0]

  const rows = frame
    ? field<string>(frame, 'Trace').map((traceId, i) => ({
        traceId,
        service: field<string>(frame, 'Service')[i],
        operation: field<string>(frame, 'Operation')[i],
        status: field<string>(frame, 'Status')[i],
        duration: field<number>(frame, 'Duration (ms)')[i],
        time: field<string>(frame, 'Time')[i],
      }))
    : []

  const visible = filter
    ? rows.filter(
        (r) =>
          r.operation.toLowerCase().includes(filter.toLowerCase()) ||
          r.service.toLowerCase().includes(filter.toLowerCase()),
      )
    : rows

  return (
    <div style={styles.page}>
      <div style={styles.header}>
        <h1 style={styles.title}>TraceForge Viewer</h1>
        <div style={styles.controls}>
          <TimeRangePicker />
          <select
            style={styles.select}
            value={service}
            onChange={(e) => setService(e.target.value)}
          >
            {SERVICES.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
          <input
            style={styles.input}
            placeholder="Filter by operation or service…"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
          />
        </div>
      </div>

      {isLoading && <div style={styles.loading}>Loading traces…</div>}

      {!isLoading && visible.length === 0 && (
        <div style={styles.empty}>No traces found. Make sure the stack is running and has traffic.</div>
      )}

      {!isLoading && visible.length > 0 && (
        <table style={styles.table}>
          <thead>
            <tr>
              {['Trace', 'Service', 'Operation', 'Status', 'Duration (ms)', 'Time'].map((col) => (
                <th key={col} style={styles.th}>
                  {col}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {visible.map((row, i) => {
              const isError = row.status.startsWith('Error')
              return (
                <tr key={i} style={isError ? { ...styles.tr, background: '#2d1515' } : styles.tr}>
                  <td style={{ ...styles.td, fontFamily: 'monospace', color: '#8b949e' }}>{row.traceId}</td>
                  <td style={styles.td}>{row.service}</td>
                  <td style={styles.td}>{row.operation}</td>
                  <td style={{ ...styles.td, color: isError ? '#f85149' : '#3fb950' }}>{row.status}</td>
                  <td style={{ ...styles.td, textAlign: 'right' }}>{row.duration}</td>
                  <td style={{ ...styles.td, color: '#8b949e' }}>{row.time}</td>
                </tr>
              )
            })}
          </tbody>
        </table>
      )}
    </div>
  )
}

export function App() {
  return (
    <SceneContextProvider timeRange={{ from: 'now-1h', to: 'now' }} withQueryController>
      <TraceTable />
    </SceneContextProvider>
  )
}

const styles = {
  page: {
    minHeight: '100vh',
    padding: '24px',
    background: '#0d1117',
    color: '#e6edf3',
  },
  header: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    flexWrap: 'wrap' as const,
    gap: '12px',
    marginBottom: '24px',
    borderBottom: '1px solid #21262d',
    paddingBottom: '16px',
  },
  title: {
    margin: 0,
    fontSize: '20px',
    fontWeight: 600,
    color: '#e6edf3',
  },
  controls: {
    display: 'flex',
    alignItems: 'center',
    gap: '10px',
    flexWrap: 'wrap' as const,
  },
  select: {
    background: '#161b22',
    border: '1px solid #30363d',
    color: '#e6edf3',
    padding: '6px 10px',
    borderRadius: '6px',
    fontSize: '14px',
    cursor: 'pointer',
  },
  input: {
    background: '#161b22',
    border: '1px solid #30363d',
    color: '#e6edf3',
    padding: '6px 10px',
    borderRadius: '6px',
    fontSize: '14px',
    width: '260px',
  },
  loading: { color: '#8b949e', padding: '32px 0', textAlign: 'center' as const },
  empty: { color: '#8b949e', padding: '32px 0', textAlign: 'center' as const },
  table: {
    width: '100%',
    borderCollapse: 'collapse' as const,
    fontSize: '14px',
  },
  th: {
    textAlign: 'left' as const,
    padding: '10px 12px',
    borderBottom: '1px solid #21262d',
    color: '#8b949e',
    fontWeight: 500,
    fontSize: '12px',
    textTransform: 'uppercase' as const,
    letterSpacing: '0.05em',
  },
  tr: {
    borderBottom: '1px solid #161b22',
  },
  td: {
    padding: '10px 12px',
    color: '#e6edf3',
  },
}
