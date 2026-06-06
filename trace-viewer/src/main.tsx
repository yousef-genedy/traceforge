import React from 'react'
import ReactDOM from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import { createBrowserHistory } from 'history'
import { LocationServiceProvider, HistoryWrapper } from '@grafana/runtime'
import { App } from './App'
import { setupJaeger } from './jaegerSource'
import { initGrafanaRuntime } from './initRuntime'

initGrafanaRuntime()
setupJaeger()

const locationService = new HistoryWrapper(createBrowserHistory())

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <BrowserRouter>
      <LocationServiceProvider service={locationService}>
        <App />
      </LocationServiceProvider>
    </BrowserRouter>
  </React.StrictMode>,
)
