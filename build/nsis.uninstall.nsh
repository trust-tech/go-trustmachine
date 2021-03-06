Section "Uninstall"
  # uninstall for all users
  setShellVarContext all

  # Delete (optionally) installed files
  {{range $}}Delete $INSTDIR\{{.}}
  {{end}}
  Delete $INSTDIR\uninstall.exe

  # Delete install directory
  rmDir $INSTDIR

  # Delete start menu launcher
  Delete "$SMPROGRAMS\${APPNAME}\${APPNAME}.lnk"
  Delete "$SMPROGRAMS\${APPNAME}\Attach.lnk"
  Delete "$SMPROGRAMS\${APPNAME}\Uninstall.lnk"
  rmDir "$SMPROGRAMS\${APPNAME}"

  # Firewall - remove rules if exists
  SimpleFC::AdvRemoveRule "Gotrust incoming peers (TCP:30303)"
  SimpleFC::AdvRemoveRule "Gotrust outgoing peers (TCP:30303)"
  SimpleFC::AdvRemoveRule "Gotrust UDP discovery (UDP:30303)"

  # Remove IPC endpoint (https://github.com/trust-tech/EIPs/issues/147)
  ${un.EnvVarUpdate} $0 "TRUSTMACHINE_SOCKET" "R" "HKLM" "\\.\pipe\gotrust.ipc"

  # Remove install directory from PATH
  Push "$INSTDIR"
  Call un.RemoveFromPath

  # Cleanup registry (deletes all sub keys)
  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${GROUPNAME} ${APPNAME}"
SectionEnd
