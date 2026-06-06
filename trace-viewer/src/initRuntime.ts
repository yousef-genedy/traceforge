import { setRunRequest, setTemplateSrv } from '@grafana/runtime'
import { LoadingState } from '@grafana/data'
import { Observable } from 'rxjs'

export function initGrafanaRuntime() {
  setTemplateSrv({
    getVariables: () => [],
    replace: (str: string) => str,
    containsTemplate: () => false,
    updateTimeRange: () => {},
  } as any)

  setRunRequest((datasource, request) => {
    const obs = new Observable<any>((subscriber) => {
      Promise.resolve(datasource.query(request))
        .then((response: any) => {
          const panelData = {
            state: response.state ?? LoadingState.Done,
            series: response.data ?? [],
            timeRange: request.range,
            request,
          }
          subscriber.next(panelData)
          subscriber.complete()
        })
        .catch((err: unknown) => subscriber.error(err))
      return () => {}
    })
    return obs as any
  })
}
