// Copyright (c) 2022 Gitpod GmbH. All rights reserved.
// Licensed under the GNU Affero General Public License (AGPL).
// See License-AGPL.txt in the project root for license information.

package io.gitpod.jetbrains.remote

import com.google.protobuf.ByteString
import com.intellij.openapi.application.runInEdt
import com.intellij.openapi.diagnostic.thisLogger
import com.intellij.openapi.project.Project
import com.intellij.openapi.wm.ToolWindowManager
import com.intellij.openapi.wm.ex.ToolWindowManagerListener
import com.jediterm.terminal.TtyConnector
import com.jetbrains.rdserver.terminal.BackendTerminalManager
import com.jetbrains.rdserver.terminal.BackendTtyConnector
import io.gitpod.supervisor.api.TerminalOuterClass
import io.gitpod.supervisor.api.TerminalServiceGrpc
import io.grpc.stub.StreamObserver
import kotlinx.coroutines.*
import kotlinx.coroutines.guava.asDeferred
import org.jetbrains.plugins.terminal.ShellTerminalWidget
import org.jetbrains.plugins.terminal.TerminalTabState
import org.jetbrains.plugins.terminal.TerminalToolWindowFactory
import org.jetbrains.plugins.terminal.TerminalView
import org.jetbrains.plugins.terminal.cloud.CloudTerminalProcess
import org.jetbrains.plugins.terminal.cloud.CloudTerminalRunner
import java.io.PipedInputStream
import java.io.PipedOutputStream

@DelicateCoroutinesApi
@Suppress("UnstableApiUsage")
class GitpodToolWindowManagerListener(private val project: Project) : ToolWindowManagerListener {
    override fun toolWindowsRegistered(ids: MutableList<String>, toolWindowManager: ToolWindowManager) {
        if (ids.contains(TerminalToolWindowFactory.TOOL_WINDOW_ID)) {
            debug("ToolWindow '${TerminalToolWindowFactory.TOOL_WINDOW_ID}' has been registered on project '${project.name}'.")
            GlobalScope.launch {
                mirrorSupervisorTerminals()
            }
        }
    }

    private val terminalView = TerminalView.getInstance(project)
    private val terminalServiceStub = TerminalServiceGrpc.newStub(GitpodManager.supervisorChannel)
    private val terminalServiceFutureStub = TerminalServiceGrpc.newFutureStub(GitpodManager.supervisorChannel)

    private suspend fun mirrorSupervisorTerminals() = coroutineScope {
        val supervisorTerminals = getSupervisorTerminalsAsync().await().terminalsList
        for (supervisorTerminal in supervisorTerminals) {
            createSharedTerminal(supervisorTerminal)
        }
    }

    private fun createSharedTerminal(supervisorTerminal: TerminalOuterClass.Terminal) = runInEdt {
        debug("Creating shared terminal '${supervisorTerminal.title}' on Backend IDE")
        val terminalInputReader = PipedInputStream()
        val terminalInputWriter = PipedOutputStream(terminalInputReader)
        val terminalOutputReader = PipedInputStream()
        val terminalOutputWriter = PipedOutputStream(terminalOutputReader)
        val terminalRunner = object : CloudTerminalRunner(project, supervisorTerminal.title, CloudTerminalProcess(terminalInputWriter, terminalOutputReader)) {
            override fun createTtyConnector(process: CloudTerminalProcess): TtyConnector {
                return BackendTtyConnector(project, super.createTtyConnector(process))
            }
        }
        terminalView.createNewSession(terminalRunner, TerminalTabState().also { it.myTabName = supervisorTerminal.title })
        val shellTerminalWidget = terminalView.widgets.find { widget ->
            terminalView.toolWindow.contentManager.getContent(widget).tabName == supervisorTerminal.title
        } as ShellTerminalWidget
        BackendTerminalManager.getInstance(project).shareTerminal(shellTerminalWidget, supervisorTerminal.alias)
        connectSupervisorStream(shellTerminalWidget, supervisorTerminal, terminalOutputWriter, terminalInputReader)
    }

    private fun connectSupervisorStream(shellTerminalWidget: ShellTerminalWidget, supervisorTerminal: TerminalOuterClass.Terminal, terminalOutputWriter: PipedOutputStream, terminalInputReader: PipedInputStream) {
        val listenTerminalRequest = TerminalOuterClass.ListenTerminalRequest.newBuilder().setAlias(supervisorTerminal.alias).build()

        val terminalResponseObserver = object : StreamObserver<TerminalOuterClass.ListenTerminalResponse> {
            override fun onNext(response: TerminalOuterClass.ListenTerminalResponse?) {
                when (response) {
                    null -> return
                    else -> when {
                        response.hasTitle() -> {
                            val shellTerminalWidgetContent = terminalView.toolWindow.contentManager.getContent(shellTerminalWidget)
                            if (shellTerminalWidgetContent.tabName != response.title) {
                                debug("Renaming '${shellTerminalWidgetContent.tabName}' to '${response.title}'.")
                                shellTerminalWidgetContent.tabName = response.title
                            }
                        }

                        response.hasData() -> {
                            debug("Printing a text on '${supervisorTerminal.title}' terminal.")
                            GlobalScope.launch {
                                withContext(Dispatchers.IO) {
                                    terminalOutputWriter.write(response.data.toByteArray())
                                    terminalOutputWriter.flush()
                                }
                            }
                        }

                        response.hasExitCode() -> {
                            debug("Closing '${supervisorTerminal.title}' terminal (Exit Code: ${response.exitCode}.")
                            shellTerminalWidget.close()
                        }
                    }
                }
            }

            override fun onCompleted() {
                debug("'${supervisorTerminal.title}' terminal finished reading stream.")
            }

            override fun onError(e: Throwable?) {
                debug("'${supervisorTerminal.title}' terminal threw error: ${e?.message}.")
            }
        }

        terminalServiceStub.listen(listenTerminalRequest, terminalResponseObserver)

        val writeTerminalResponseObserver = object : StreamObserver<TerminalOuterClass.WriteTerminalResponse> {
            override fun onNext(response: TerminalOuterClass.WriteTerminalResponse?) {
                if (response != null) {
                    debug("bytesWritten = ${response.bytesWritten}")
                }
            }

            override fun onError(e: Throwable?) {
                debug("'${supervisorTerminal.title}' terminal threw error: ${e?.message}.")
            }

            override fun onCompleted() {
                debug("'${supervisorTerminal.title}' terminal finished writing stream.")
            }

        }

        val writeTerminalRequestBuilder = TerminalOuterClass
                .WriteTerminalRequest
                .newBuilder()
                .setAlias(supervisorTerminal.alias)

        val watchTerminalInputJob = GlobalScope.launch {
            withContext(Dispatchers.IO) {
                while (isActive) {
                    val bytesAvailableOnTerminalInput = terminalInputReader.available()

                    if (bytesAvailableOnTerminalInput > 0) {
                        val bytesFromTerminalInput = ByteArray(bytesAvailableOnTerminalInput)

                        terminalInputReader.read(bytesFromTerminalInput, 0, bytesFromTerminalInput.size)

                        val supervisorTerminalStdin = ByteString.copyFrom(bytesFromTerminalInput)
                        val writeTerminalRequest = writeTerminalRequestBuilder.setStdin(supervisorTerminalStdin).build()

                        terminalServiceStub.write(writeTerminalRequest, writeTerminalResponseObserver)
                    }
                }
            }
        }

        shellTerminalWidget.addListener {
            debug("Terminal '${supervisorTerminal.title}' was closed on IDE")
            watchTerminalInputJob.cancel()
            val shutdownTerminalRequest = TerminalOuterClass.ShutdownTerminalRequest.newBuilder().setAlias(supervisorTerminal.alias).build()
            terminalServiceStub.shutdown(shutdownTerminalRequest, null)
        }
    }

    private fun getSupervisorTerminalsAsync() = terminalServiceFutureStub
            .list(TerminalOuterClass.ListTerminalsRequest.newBuilder().build())
            .asDeferred()

    private fun debug(message: String) {
        if (System.getenv("JB_DEV").toBoolean())
            thisLogger().warn(message)
    }
}
