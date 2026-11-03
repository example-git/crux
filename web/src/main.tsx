import { render } from 'solid-js/web';
import { App } from './app.tsx';
import './style.css';

if (window.cruxDesktop) document.documentElement.classList.add('desktop');

render(() => <App />, document.getElementById('root')!);
