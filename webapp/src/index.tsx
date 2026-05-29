// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import manifest from 'manifest';
import React from 'react';
import type {Store} from 'redux';

import type {GlobalState} from '@mattermost/types/store';

import type {PluginRegistry} from 'types/mattermost-webapp';

import HostTerminal from './components/terminal';

export default class Plugin {
    public async initialize(registry: PluginRegistry, store: Store<GlobalState>) {
        // @see https://developers.mattermost.com/extend/plugins/webapp/reference/
        const {toggleRHSPlugin} = registry.registerRightHandSidebarComponent(
            HostTerminal,
            'Host Terminal',
        );

        registry.registerChannelHeaderButtonAction(
            <span style={{fontWeight: 700, fontFamily: 'monospace'}}>{'>_'}</span>,
            () => store.dispatch(toggleRHSPlugin),
            'Host Terminal',
            'Open an interactive host terminal (system admins only)',
        );
    }
}

declare global {
    interface Window {
        registerPlugin(pluginId: string, plugin: Plugin): void;
    }
}

window.registerPlugin(manifest.id, new Plugin());
