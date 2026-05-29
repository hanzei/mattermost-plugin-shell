// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {FitAddon} from '@xterm/addon-fit';
import {Terminal} from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import manifest from 'manifest';
import React, {useCallback, useEffect, useRef, useState} from 'react';

// Output flows backend -> browser as raw binary frames written straight to
// xterm. Control flows browser -> backend as JSON envelopes.
type ClientMessage =
    | {type: 'data'; data: string}
    | {type: 'resize'; cols: number; rows: number};

function terminalSocketURL(): string {
    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    return `${proto}//${window.location.host}/plugins/${manifest.id}/api/v1/terminal`;
}

const HostTerminal: React.FC = () => {
    const containerRef = useRef<HTMLDivElement | null>(null);
    const termRef = useRef<Terminal | null>(null);
    const fitRef = useRef<FitAddon | null>(null);
    const socketRef = useRef<WebSocket | null>(null);

    const [fullscreen, setFullscreen] = useState(false);
    const [status, setStatus] = useState<'connecting' | 'open' | 'closed'>('connecting');

    const send = useCallback((msg: ClientMessage) => {
        const socket = socketRef.current;
        if (socket && socket.readyState === WebSocket.OPEN) {
            socket.send(JSON.stringify(msg));
        }
    }, []);

    const fitAndResize = useCallback(() => {
        const fit = fitRef.current;
        const term = termRef.current;
        if (!fit || !term) {
            return;
        }
        try {
            fit.fit();
        } catch {
            return; // container not laid out yet
        }
        send({type: 'resize', cols: term.cols, rows: term.rows});
    }, [send]);

    useEffect(() => {
        if (!containerRef.current) {
            return undefined;
        }

        const term = new Terminal({
            cursorBlink: true,
            fontSize: 13,
            fontFamily: 'Menlo, Monaco, Consolas, "Courier New", monospace',
            theme: {background: '#1e1e1e'},
        });
        const fit = new FitAddon();
        term.loadAddon(fit);
        term.open(containerRef.current);
        fit.fit();

        termRef.current = term;
        fitRef.current = fit;

        const socket = new WebSocket(terminalSocketURL());
        socket.binaryType = 'arraybuffer';
        socketRef.current = socket;

        socket.onopen = () => {
            setStatus('open');
            send({type: 'resize', cols: term.cols, rows: term.rows});
            term.focus();
        };
        socket.onmessage = (event: MessageEvent) => {
            if (event.data instanceof ArrayBuffer) {
                term.write(new Uint8Array(event.data));
            } else if (typeof event.data === 'string') {
                term.write(event.data);
            }
        };
        socket.onclose = () => {
            setStatus('closed');
            term.write('\r\n\x1b[31m[session closed]\x1b[0m\r\n');
        };

        const dataSub = term.onData((data) => send({type: 'data', data}));
        const onWindowResize = () => fitAndResize();
        window.addEventListener('resize', onWindowResize);

        return () => {
            window.removeEventListener('resize', onWindowResize);
            dataSub.dispose();
            socket.close();
            term.dispose();
            socketRef.current = null;
            termRef.current = null;
            fitRef.current = null;
        };
    }, [send, fitAndResize]);

    // Refit after the container changes size (fullscreen toggle).
    useEffect(() => {
        const id = window.setTimeout(fitAndResize, 50);
        return () => window.clearTimeout(id);
    }, [fullscreen, fitAndResize]);

    const wrapperStyle: React.CSSProperties = fullscreen ? {
        position: 'fixed',
        inset: 0,
        zIndex: 9999,
        display: 'flex',
        flexDirection: 'column',
        background: '#1e1e1e',
    } : {
        display: 'flex',
        flexDirection: 'column',
        height: '100%',
        background: '#1e1e1e',
    };

    return (
        <div style={wrapperStyle}>
            <div
                style={{
                    display: 'flex',
                    alignItems: 'center',
                    justifyContent: 'space-between',
                    padding: '6px 10px',
                    color: '#ddd',
                    fontSize: 12,
                    borderBottom: '1px solid #333',
                }}
            >
                <span>{'Host terminal · '}{status}</span>
                <button
                    type='button'
                    onClick={() => setFullscreen((v) => !v)}
                    style={{
                        background: 'transparent',
                        border: '1px solid #555',
                        borderRadius: 4,
                        color: '#ddd',
                        cursor: 'pointer',
                        padding: '2px 8px',
                    }}
                >
                    {fullscreen ? 'Exit full screen' : 'Full screen'}
                </button>
            </div>
            <div
                ref={containerRef}
                style={{flex: 1, minHeight: 0, padding: 4}}
            />
        </div>
    );
};

export default HostTerminal;
