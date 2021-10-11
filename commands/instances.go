// This file is part of arduino-cli.
//
// Copyright 2020 ARDUINO SA (http://www.arduino.cc/)
//
// This software is released under the GNU General Public License version 3,
// which covers the main part of arduino-cli.
// The terms of this license can be found at:
// https://www.gnu.org/licenses/gpl-3.0.en.html
//
// You can be released from the requirements of the above licenses by purchasing
// a commercial license. Buying such a license is mandatory if you want to
// modify or otherwise use the software for commercial activities involving the
// Arduino software without disclosing the source code of your own applications.
// To purchase a commercial license, send an email to license@arduino.cc.

package commands

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path"

	"github.com/arduino/arduino-cli/arduino/cores"
	"github.com/arduino/arduino-cli/arduino/cores/packageindex"
	"github.com/arduino/arduino-cli/arduino/cores/packagemanager"
	"github.com/arduino/arduino-cli/arduino/libraries"
	"github.com/arduino/arduino-cli/arduino/libraries/librariesindex"
	"github.com/arduino/arduino-cli/arduino/libraries/librariesmanager"
	"github.com/arduino/arduino-cli/arduino/security"
	sk "github.com/arduino/arduino-cli/arduino/sketch"
	"github.com/arduino/arduino-cli/arduino/utils"
	"github.com/arduino/arduino-cli/cli/globals"
	"github.com/arduino/arduino-cli/configuration"
	rpc "github.com/arduino/arduino-cli/rpc/cc/arduino/cli/commands/v1"
	paths "github.com/arduino/go-paths-helper"
	"github.com/sirupsen/logrus"
	"go.bug.st/downloader/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// this map contains all the running Arduino Core Services instances
// referenced by an int32 handle
var instances = map[int32]*CoreInstance{}
var instancesCount int32 = 1

// CoreInstance is an instance of the Arduino Core Services. The user can
// instantiate as many as needed by providing a different configuration
// for each one.
type CoreInstance struct {
	PackageManager *packagemanager.PackageManager
	lm             *librariesmanager.LibrariesManager
}

// InstanceContainer FIXMEDOC
type InstanceContainer interface {
	GetInstance() *rpc.Instance
}

// GetInstance returns a CoreInstance for the given ID, or nil if ID
// doesn't exist
func GetInstance(id int32) *CoreInstance {
	return instances[id]
}

// GetPackageManager returns a PackageManager for the given ID, or nil if
// ID doesn't exist
func GetPackageManager(id int32) *packagemanager.PackageManager {
	i, ok := instances[id]
	if !ok {
		return nil
	}
	return i.PackageManager
}

// GetLibraryManager returns the library manager for the given instance ID
func GetLibraryManager(instanceID int32) *librariesmanager.LibrariesManager {
	i, ok := instances[instanceID]
	if !ok {
		return nil
	}
	return i.lm
}

func (instance *CoreInstance) installToolIfMissing(tool *cores.ToolRelease, downloadCB DownloadProgressCB, taskCB TaskProgressCB) (bool, error) {
	if tool.IsInstalled() {
		return false, nil
	}
	taskCB(&rpc.TaskProgress{Name: tr("Downloading missing tool %s", tool)})
	if err := DownloadToolRelease(instance.PackageManager, tool, downloadCB); err != nil {
		return false, fmt.Errorf(tr("downloading %[1]s tool: %[2]s"), tool, err)
	}
	taskCB(&rpc.TaskProgress{Completed: true})
	if err := InstallToolRelease(instance.PackageManager, tool, taskCB); err != nil {
		return false, fmt.Errorf(tr("installing %[1]s tool: %[2]s"), tool, err)
	}
	return true, nil
}

// Create a new CoreInstance ready to be initialized, supporting directories are also created.
func Create(req *rpc.CreateRequest) (*rpc.CreateResponse, error) {
	instance := &CoreInstance{}

	// Setup downloads directory
	downloadsDir := paths.New(configuration.Settings.GetString("directories.Downloads"))
	if downloadsDir.NotExist() {
		err := downloadsDir.MkdirAll()
		if err != nil {
			return nil, &PermissionDeniedError{Message: tr("Failed to create downloads directory"), Cause: err}
		}
	}

	// Setup data directory
	dataDir := paths.New(configuration.Settings.GetString("directories.Data"))
	packagesDir := configuration.PackagesDir(configuration.Settings)
	if packagesDir.NotExist() {
		err := packagesDir.MkdirAll()
		if err != nil {
			return nil, &PermissionDeniedError{Message: tr("Failed to create data directory"), Cause: err}
		}
	}

	// Create package manager
	instance.PackageManager = packagemanager.NewPackageManager(
		dataDir,
		configuration.PackagesDir(configuration.Settings),
		downloadsDir,
		dataDir.Join("tmp"),
	)

	// Create library manager and add libraries directories
	instance.lm = librariesmanager.NewLibraryManager(
		dataDir,
		downloadsDir,
	)

	// Add directories of libraries bundled with IDE
	if bundledLibsDir := configuration.IDEBundledLibrariesDir(configuration.Settings); bundledLibsDir != nil {
		instance.lm.AddLibrariesDir(bundledLibsDir, libraries.IDEBuiltIn)
	}

	// Add libraries directory from config file
	instance.lm.AddLibrariesDir(
		configuration.LibrariesDir(configuration.Settings),
		libraries.User,
	)

	// Save instance
	instanceID := instancesCount
	instances[instanceID] = instance
	instancesCount++

	return &rpc.CreateResponse{
		Instance: &rpc.Instance{Id: instanceID},
	}, nil
}

// Init loads installed libraries and Platforms in CoreInstance with specified ID,
// a gRPC status error is returned if the CoreInstance doesn't exist.
// All responses are sent through responseCallback, can be nil to ignore all responses.
// Failures don't stop the loading process, in case of loading failure the Platform or library
// is simply skipped and an error gRPC status is sent to responseCallback.
func Init(req *rpc.InitRequest, responseCallback func(r *rpc.InitResponse)) error {
	if responseCallback == nil {
		responseCallback = func(r *rpc.InitResponse) {}
	}
	instance := instances[req.Instance.Id]
	if instance == nil {
		return &InvalidInstanceError{}
	}

	// We need to clear the PackageManager currently in use by this instance
	// in case this is not the first Init on this instances, that might happen
	// after reinitializing an instance after installing or uninstalling a core.
	// If this is not done the information of the uninstall core is kept in memory,
	// even if it should not.
	instance.PackageManager.Clear()
	ctagsTool := getBuiltinCtagsTool(instance.PackageManager)
	serialDiscoveryTool := getBuiltinSerialDiscoveryTool(instance.PackageManager)
	mdnsDiscoveryTool := getBuiltinMDNSDiscoveryTool(instance.PackageManager)
	serialMonitorRool := getBuiltinSerialMonitorTool(instance.PackageManager)

	// Load Platforms
	urls := []string{globals.DefaultIndexURL}
	urls = append(urls, configuration.Settings.GetStringSlice("board_manager.additional_urls")...)
	for _, u := range urls {
		URL, err := utils.URLParse(u)
		if err != nil {
			s := status.Newf(codes.InvalidArgument, tr("Invalid additional URL: %v"), err)
			responseCallback(&rpc.InitResponse{
				Message: &rpc.InitResponse_Error{
					Error: s.Proto(),
				},
			})
			continue
		}

		if URL.Scheme == "file" {
			indexFile := paths.New(URL.Path)

			_, err := instance.PackageManager.LoadPackageIndexFromFile(indexFile)
			if err != nil {
				s := status.Newf(codes.FailedPrecondition, tr("Loading index file: %v"), err)
				responseCallback(&rpc.InitResponse{
					Message: &rpc.InitResponse_Error{
						Error: s.Proto(),
					},
				})
			}
			continue
		}

		if err := instance.PackageManager.LoadPackageIndex(URL); err != nil {
			s := status.Newf(codes.FailedPrecondition, tr("Loading index file: %v"), err)
			responseCallback(&rpc.InitResponse{
				Message: &rpc.InitResponse_Error{
					Error: s.Proto(),
				},
			})
		}
	}

	// We load hardware before verifying builtin tools are installed
	// otherwise we wouldn't find them and reinstall them each time
	// and they would never get reloaded.
	for _, err := range instance.PackageManager.LoadHardware() {
		responseCallback(&rpc.InitResponse{
			Message: &rpc.InitResponse_Error{
				Error: err.Proto(),
			},
		})
	}

	taskCallback := func(msg *rpc.TaskProgress) {
		responseCallback(&rpc.InitResponse{
			Message: &rpc.InitResponse_InitProgress{
				InitProgress: &rpc.InitResponse_Progress{
					TaskProgress: msg,
				},
			},
		})
	}

	downloadCallback := func(msg *rpc.DownloadProgress) {
		responseCallback(&rpc.InitResponse{
			Message: &rpc.InitResponse_InitProgress{
				InitProgress: &rpc.InitResponse_Progress{
					DownloadProgress: msg,
				},
			},
		})
	}

	// Install tools if necessary
	ctagsHasBeenInstalled, err := instance.installToolIfMissing(ctagsTool, downloadCallback, taskCallback)
	if err != nil {
		s := status.Newf(codes.Internal, err.Error())
		responseCallback(&rpc.InitResponse{
			Message: &rpc.InitResponse_Error{
				Error: s.Proto(),
			},
		})
	}

	serialDiscoveryHasBeenInstalled, err := instance.installToolIfMissing(serialDiscoveryTool, downloadCallback, taskCallback)
	if err != nil {
		s := status.Newf(codes.Internal, err.Error())
		responseCallback(&rpc.InitResponse{
			Message: &rpc.InitResponse_Error{
				Error: s.Proto(),
			},
		})
	}

	mdnsDiscoveryHasBeenInstalled, err := instance.installToolIfMissing(mdnsDiscoveryTool, downloadCallback, taskCallback)
	if err != nil {
		s := status.Newf(codes.Internal, err.Error())
		responseCallback(&rpc.InitResponse{
			Message: &rpc.InitResponse_Error{
				Error: s.Proto(),
			},
		})
	}

	serialMonitorHasBeenInstalled, err := instance.installToolIfMissing(serialMonitorRool, downloadCallback, taskCallback)
	if err != nil {
		s := status.Newf(codes.Internal, err.Error())
		responseCallback(&rpc.InitResponse{
			Message: &rpc.InitResponse_Error{
				Error: s.Proto(),
			},
		})
	}

	if ctagsHasBeenInstalled || serialDiscoveryHasBeenInstalled || mdnsDiscoveryHasBeenInstalled || serialMonitorHasBeenInstalled {
		// We installed at least one new tool after loading hardware
		// so we must reload again otherwise we would never found them.
		for _, err := range instance.PackageManager.LoadHardware() {
			responseCallback(&rpc.InitResponse{
				Message: &rpc.InitResponse_Error{
					Error: err.Proto(),
				},
			})
		}
	}

	for _, err := range instance.PackageManager.LoadDiscoveries() {
		responseCallback(&rpc.InitResponse{
			Message: &rpc.InitResponse_Error{
				Error: err.Proto(),
			},
		})
	}

	// Load libraries
	for _, pack := range instance.PackageManager.Packages {
		for _, platform := range pack.Platforms {
			if platformRelease := instance.PackageManager.GetInstalledPlatformRelease(platform); platformRelease != nil {
				instance.lm.AddPlatformReleaseLibrariesDir(platformRelease, libraries.PlatformBuiltIn)
			}
		}
	}

	if err := instance.lm.LoadIndex(); err != nil {
		s := status.Newf(codes.FailedPrecondition, tr("Loading index file: %v"), err)
		responseCallback(&rpc.InitResponse{
			Message: &rpc.InitResponse_Error{
				Error: s.Proto(),
			},
		})
	}

	for _, err := range instance.lm.RescanLibraries() {
		s := status.Newf(codes.FailedPrecondition, tr("Loading libraries: %v"), err)
		responseCallback(&rpc.InitResponse{
			Message: &rpc.InitResponse_Error{
				Error: s.Proto(),
			},
		})
	}

	return nil
}

// Destroy FIXMEDOC
func Destroy(ctx context.Context, req *rpc.DestroyRequest) (*rpc.DestroyResponse, error) {
	id := req.GetInstance().GetId()
	if _, ok := instances[id]; !ok {
		return nil, &InvalidInstanceError{}
	}

	delete(instances, id)
	return &rpc.DestroyResponse{}, nil
}

// UpdateLibrariesIndex updates the library_index.json
func UpdateLibrariesIndex(ctx context.Context, req *rpc.UpdateLibrariesIndexRequest, downloadCB func(*rpc.DownloadProgress)) error {
	logrus.Info("Updating libraries index")
	lm := GetLibraryManager(req.GetInstance().GetId())
	if lm == nil {
		return &InvalidInstanceError{}
	}
	config, err := GetDownloaderConfig()
	if err != nil {
		return err
	}

	if err := lm.IndexFile.Parent().MkdirAll(); err != nil {
		return &PermissionDeniedError{Message: tr("Could not create index directory"), Cause: err}
	}

	// Create a temp dir to stage all downloads
	tmp, err := paths.MkTempDir("", "library_index_download")
	if err != nil {
		return &TempDirCreationFailedError{Cause: err}
	}
	defer tmp.RemoveAll()

	// Download gzipped library_index
	tmpIndexGz := tmp.Join("library_index.json.gz")
	if d, err := downloader.DownloadWithConfig(tmpIndexGz.String(), librariesmanager.LibraryIndexGZURL.String(), *config, downloader.NoResume); err == nil {
		if err := Download(d, tr("Updating index: library_index.json.gz"), downloadCB); err != nil {
			return &FailedDownloadError{Message: tr("Error downloading library_index.json.gz"), Cause: err}
		}
	} else {
		return &FailedDownloadError{Message: tr("Error downloading library_index.json.gz"), Cause: err}
	}

	// Download signature
	tmpSignature := tmp.Join("library_index.json.sig")
	if d, err := downloader.DownloadWithConfig(tmpSignature.String(), librariesmanager.LibraryIndexSignature.String(), *config, downloader.NoResume); err == nil {
		if err := Download(d, tr("Updating index: library_index.json.sig"), downloadCB); err != nil {
			return &FailedDownloadError{Message: tr("Error downloading library_index.json.sig"), Cause: err}
		}
	} else {
		return &FailedDownloadError{Message: tr("Error downloading library_index.json.sig"), Cause: err}
	}

	// Extract the real library_index
	tmpIndex := tmp.Join("library_index.json")
	if err := paths.GUnzip(tmpIndexGz, tmpIndex); err != nil {
		return &PermissionDeniedError{Message: tr("Error extracting library_index.json.gz"), Cause: err}
	}

	// Check signature
	if ok, _, err := security.VerifyArduinoDetachedSignature(tmpIndex, tmpSignature); err != nil {
		return &PermissionDeniedError{Message: tr("Error verifying signature"), Cause: err}
	} else if !ok {
		return &SignatureVerificationFailedError{File: "library_index.json"}
	}

	// Copy extracted library_index and signature to final destination
	lm.IndexFile.Remove()
	lm.IndexFileSignature.Remove()
	if err := tmpIndex.CopyTo(lm.IndexFile); err != nil {
		return &PermissionDeniedError{Message: tr("Error writing library_index.json"), Cause: err}
	}
	if err := tmpSignature.CopyTo(lm.IndexFileSignature); err != nil {
		return &PermissionDeniedError{Message: tr("Error writing library_index.json.sig"), Cause: err}
	}

	return nil
}

// UpdateIndex FIXMEDOC
func UpdateIndex(ctx context.Context, req *rpc.UpdateIndexRequest, downloadCB DownloadProgressCB) (*rpc.UpdateIndexResponse, error) {
	id := req.GetInstance().GetId()
	_, ok := instances[id]
	if !ok {
		return nil, &InvalidInstanceError{}
	}

	indexpath := paths.New(configuration.Settings.GetString("directories.Data"))

	urls := []string{globals.DefaultIndexURL}
	urls = append(urls, configuration.Settings.GetStringSlice("board_manager.additional_urls")...)
	for _, u := range urls {
		logrus.Info("URL: ", u)
		URL, err := utils.URLParse(u)
		if err != nil {
			logrus.Warnf("unable to parse additional URL: %s", u)
			continue
		}

		logrus.WithField("url", URL).Print("Updating index")

		if URL.Scheme == "file" {
			path := paths.New(URL.Path)
			if _, err := packageindex.LoadIndexNoSign(path); err != nil {
				return nil, &InvalidArgumentError{Message: tr("Invalid package index in %s", path), Cause: err}
			}

			fi, _ := os.Stat(path.String())
			downloadCB(&rpc.DownloadProgress{
				File:      tr("Updating index: %s", path.Base()),
				TotalSize: fi.Size(),
			})
			downloadCB(&rpc.DownloadProgress{Completed: true})
			continue
		}

		var tmp *paths.Path
		if tmpFile, err := ioutil.TempFile("", ""); err != nil {
			return nil, &TempFileCreationFailedError{Cause: err}
		} else if err := tmpFile.Close(); err != nil {
			return nil, &TempFileCreationFailedError{Cause: err}
		} else {
			tmp = paths.New(tmpFile.Name())
		}
		defer tmp.Remove()

		config, err := GetDownloaderConfig()
		if err != nil {
			return nil, &FailedDownloadError{Message: tr("Error downloading index '%s'", URL), Cause: err}
		}
		d, err := downloader.DownloadWithConfig(tmp.String(), URL.String(), *config)
		if err != nil {
			return nil, &FailedDownloadError{Message: tr("Error downloading index '%s'", URL), Cause: err}
		}
		coreIndexPath := indexpath.Join(path.Base(URL.Path))
		err = Download(d, tr("Updating index: %s", coreIndexPath.Base()), downloadCB)
		if err != nil {
			return nil, &FailedDownloadError{Message: tr("Error downloading index '%s'", URL), Cause: err}
		}

		// Check for signature
		var tmpSig *paths.Path
		var coreIndexSigPath *paths.Path
		if URL.Hostname() == "downloads.arduino.cc" {
			URLSig, err := url.Parse(URL.String())
			if err != nil {
				return nil, &InvalidURLError{Cause: err}
			}
			URLSig.Path += ".sig"

			if t, err := ioutil.TempFile("", ""); err != nil {
				return nil, &TempFileCreationFailedError{Cause: err}
			} else if err := t.Close(); err != nil {
				return nil, &TempFileCreationFailedError{Cause: err}
			} else {
				tmpSig = paths.New(t.Name())
			}
			defer tmpSig.Remove()

			d, err := downloader.DownloadWithConfig(tmpSig.String(), URLSig.String(), *config)
			if err != nil {
				return nil, &FailedDownloadError{Message: tr("Error downloading index signature '%s'", URLSig), Cause: err}
			}

			coreIndexSigPath = indexpath.Join(path.Base(URLSig.Path))
			Download(d, tr("Updating index: %s", coreIndexSigPath.Base()), downloadCB)
			if d.Error() != nil {
				return nil, &FailedDownloadError{Message: tr("Error downloading index signature '%s'", URLSig), Cause: err}
			}

			if valid, _, err := security.VerifyArduinoDetachedSignature(tmp, tmpSig); err != nil {
				return nil, &PermissionDeniedError{Message: tr("Error verifying signature"), Cause: err}
			} else if !valid {
				return nil, &SignatureVerificationFailedError{File: URL.String()}
			}
		}

		if _, err := packageindex.LoadIndex(tmp); err != nil {
			return nil, &InvalidArgumentError{Message: tr("Invalid package index in %s", URL), Cause: err}
		}

		if err := indexpath.MkdirAll(); err != nil {
			return nil, &PermissionDeniedError{Message: tr("Can't create data directory %s", indexpath), Cause: err}
		}

		if err := tmp.CopyTo(coreIndexPath); err != nil {
			return nil, &PermissionDeniedError{Message: tr("Error saving downloaded index %s", URL), Cause: err}
		}
		if tmpSig != nil {
			if err := tmpSig.CopyTo(coreIndexSigPath); err != nil {
				return nil, &PermissionDeniedError{Message: tr("Error saving downloaded index signature"), Cause: err}
			}
		}
	}

	return &rpc.UpdateIndexResponse{}, nil
}

// UpdateCoreLibrariesIndex updates both Cores and Libraries indexes
func UpdateCoreLibrariesIndex(ctx context.Context, req *rpc.UpdateCoreLibrariesIndexRequest, downloadCB DownloadProgressCB) error {
	_, err := UpdateIndex(ctx, &rpc.UpdateIndexRequest{
		Instance: req.Instance,
	}, downloadCB)
	if err != nil {
		return err
	}

	err = UpdateLibrariesIndex(ctx, &rpc.UpdateLibrariesIndexRequest{
		Instance: req.Instance,
	}, downloadCB)
	if err != nil {
		return err
	}

	return nil
}

// Outdated returns a list struct containing both Core and Libraries that can be updated
func Outdated(ctx context.Context, req *rpc.OutdatedRequest) (*rpc.OutdatedResponse, error) {
	id := req.GetInstance().GetId()

	lm := GetLibraryManager(id)
	if lm == nil {
		return nil, &InvalidInstanceError{}
	}

	outdatedLibraries := []*rpc.InstalledLibrary{}
	for _, libAlternatives := range lm.Libraries {
		for _, library := range libAlternatives.Alternatives {
			if library.Location != libraries.User {
				continue
			}
			available := lm.Index.FindLibraryUpdate(library)
			if available == nil {
				continue
			}

			outdatedLibraries = append(outdatedLibraries, &rpc.InstalledLibrary{
				Library: getOutputLibrary(library),
				Release: getOutputRelease(available),
			})
		}
	}

	pm := GetPackageManager(id)
	if pm == nil {
		return nil, &InvalidInstanceError{}
	}

	outdatedPlatforms := []*rpc.Platform{}
	for _, targetPackage := range pm.Packages {
		for _, installed := range targetPackage.Platforms {
			if installedRelease := pm.GetInstalledPlatformRelease(installed); installedRelease != nil {
				latest := installed.GetLatestRelease()
				if latest == nil || latest == installedRelease {
					continue
				}
				rpcPlatform := PlatformReleaseToRPC(latest)
				rpcPlatform.Installed = installedRelease.Version.String()

				outdatedPlatforms = append(
					outdatedPlatforms,
					rpcPlatform,
				)
			}
		}
	}

	return &rpc.OutdatedResponse{
		OutdatedLibraries: outdatedLibraries,
		OutdatedPlatforms: outdatedPlatforms,
	}, nil
}

func getOutputLibrary(lib *libraries.Library) *rpc.Library {
	insdir := ""
	if lib.InstallDir != nil {
		insdir = lib.InstallDir.String()
	}
	srcdir := ""
	if lib.SourceDir != nil {
		srcdir = lib.SourceDir.String()
	}
	utldir := ""
	if lib.UtilityDir != nil {
		utldir = lib.UtilityDir.String()
	}
	cntplat := ""
	if lib.ContainerPlatform != nil {
		cntplat = lib.ContainerPlatform.String()
	}

	return &rpc.Library{
		Name:              lib.Name,
		Author:            lib.Author,
		Maintainer:        lib.Maintainer,
		Sentence:          lib.Sentence,
		Paragraph:         lib.Paragraph,
		Website:           lib.Website,
		Category:          lib.Category,
		Architectures:     lib.Architectures,
		Types:             lib.Types,
		InstallDir:        insdir,
		SourceDir:         srcdir,
		UtilityDir:        utldir,
		Location:          lib.Location.ToRPCLibraryLocation(),
		ContainerPlatform: cntplat,
		Layout:            lib.Layout.ToRPCLibraryLayout(),
		RealName:          lib.RealName,
		DotALinkage:       lib.DotALinkage,
		Precompiled:       lib.Precompiled,
		LdFlags:           lib.LDflags,
		IsLegacy:          lib.IsLegacy,
		Version:           lib.Version.String(),
		License:           lib.License,
	}
}

func getOutputRelease(lib *librariesindex.Release) *rpc.LibraryRelease {
	if lib != nil {
		return &rpc.LibraryRelease{
			Author:        lib.Author,
			Version:       lib.Version.String(),
			Maintainer:    lib.Maintainer,
			Sentence:      lib.Sentence,
			Paragraph:     lib.Paragraph,
			Website:       lib.Website,
			Category:      lib.Category,
			Architectures: lib.Architectures,
			Types:         lib.Types,
		}
	}
	return &rpc.LibraryRelease{}
}

// Upgrade downloads and installs outdated Cores and Libraries
func Upgrade(ctx context.Context, req *rpc.UpgradeRequest, downloadCB DownloadProgressCB, taskCB TaskProgressCB) error {
	downloaderConfig, err := GetDownloaderConfig()
	if err != nil {
		return err
	}

	lm := GetLibraryManager(req.Instance.Id)
	if lm == nil {
		return &InvalidInstanceError{}
	}

	for _, libAlternatives := range lm.Libraries {
		for _, library := range libAlternatives.Alternatives {
			if library.Location != libraries.User {
				continue
			}
			available := lm.Index.FindLibraryUpdate(library)
			if available == nil {
				continue
			}

			// Downloads latest library release
			taskCB(&rpc.TaskProgress{Name: tr("Downloading %s", available)})
			if d, err := available.Resource.Download(lm.DownloadsDir, downloaderConfig); err != nil {
				return &FailedDownloadError{Message: tr("Error downloading library"), Cause: err}
			} else if err := Download(d, available.String(), downloadCB); err != nil {
				return &FailedDownloadError{Message: tr("Error downloading library"), Cause: err}
			}

			// Installs downloaded library
			taskCB(&rpc.TaskProgress{Name: tr("Installing %s", available)})
			libPath, libReplaced, err := lm.InstallPrerequisiteCheck(available)
			if errors.Is(err, librariesmanager.ErrAlreadyInstalled) {
				taskCB(&rpc.TaskProgress{Message: tr("Already installed %s", available), Completed: true})
				continue
			} else if err != nil {
				return &FailedLibraryInstallError{Cause: err}
			}

			if libReplaced != nil {
				taskCB(&rpc.TaskProgress{Message: tr("Replacing %[1]s with %[2]s", libReplaced, available)})
			}

			if err := lm.Install(available, libPath); err != nil {
				return &FailedLibraryInstallError{Cause: err}
			}

			taskCB(&rpc.TaskProgress{Message: tr("Installed %s", available), Completed: true})
		}
	}

	pm := GetPackageManager(req.Instance.Id)
	if pm == nil {
		return &InvalidInstanceError{}
	}

	for _, targetPackage := range pm.Packages {
		for _, installed := range targetPackage.Platforms {
			if installedRelease := pm.GetInstalledPlatformRelease(installed); installedRelease != nil {
				latest := installed.GetLatestRelease()
				if latest == nil || latest == installedRelease {
					continue
				}

				ref := &packagemanager.PlatformReference{
					Package:              installedRelease.Platform.Package.Name,
					PlatformArchitecture: installedRelease.Platform.Architecture,
					PlatformVersion:      installedRelease.Version,
				}
				// Get list of installed tools needed by the currently installed version
				_, installedTools, err := pm.FindPlatformReleaseDependencies(ref)
				if err != nil {
					return &NotFoundError{Message: tr("Can't find dependencies for platform %s", ref), Cause: err}
				}

				ref = &packagemanager.PlatformReference{
					Package:              latest.Platform.Package.Name,
					PlatformArchitecture: latest.Platform.Architecture,
					PlatformVersion:      latest.Version,
				}

				taskCB(&rpc.TaskProgress{Name: tr("Downloading %s", latest)})
				_, tools, err := pm.FindPlatformReleaseDependencies(ref)
				if err != nil {
					return &NotFoundError{Message: tr("Can't find dependencies for platform %s", ref), Cause: err}
				}

				toolsToInstall := []*cores.ToolRelease{}
				for _, tool := range tools {
					if tool.IsInstalled() {
						logrus.WithField("tool", tool).Warn("Tool already installed")
						taskCB(&rpc.TaskProgress{Name: tr("Tool %s already installed", tool), Completed: true})
					} else {
						toolsToInstall = append(toolsToInstall, tool)
					}
				}

				// Downloads platform tools
				for _, tool := range toolsToInstall {
					if err := DownloadToolRelease(pm, tool, downloadCB); err != nil {
						taskCB(&rpc.TaskProgress{Message: tr("Error downloading tool %s", tool)})
						return &FailedDownloadError{Message: tr("Error downloading tool %s", tool), Cause: err}
					}
				}

				// Downloads platform
				if d, err := pm.DownloadPlatformRelease(latest, downloaderConfig); err != nil {
					return &FailedDownloadError{Message: tr("Error downloading platform %s", latest), Cause: err}
				} else if err := Download(d, latest.String(), downloadCB); err != nil {
					return &FailedDownloadError{Message: tr("Error downloading platform %s", latest), Cause: err}
				}

				logrus.Info("Updating platform " + installed.String())
				taskCB(&rpc.TaskProgress{Name: tr("Updating platform %s", latest)})

				// Installs tools
				for _, tool := range toolsToInstall {
					if err := InstallToolRelease(pm, tool, taskCB); err != nil {
						msg := tr("Error installing tool %s", tool)
						taskCB(&rpc.TaskProgress{Message: msg})
						return &FailedInstallError{Message: msg, Cause: err}
					}
				}

				// Installs platform
				err = pm.InstallPlatform(latest)
				if err != nil {
					logrus.WithError(err).Error("Cannot install platform")
					msg := tr("Error installing platform %s", latest)
					taskCB(&rpc.TaskProgress{Message: msg})
					return &FailedInstallError{Message: msg, Cause: err}
				}

				// Uninstall previously installed release
				err = pm.UninstallPlatform(installedRelease)

				// In case uninstall fails tries to rollback
				if err != nil {
					logrus.WithError(err).Error("Error updating platform.")
					taskCB(&rpc.TaskProgress{Message: tr("Error upgrading platform: %s", err)})

					// Rollback
					if err := pm.UninstallPlatform(latest); err != nil {
						logrus.WithError(err).Error("Error rolling-back changes.")
						msg := tr("Error rolling-back changes")
						taskCB(&rpc.TaskProgress{Message: fmt.Sprintf("%s: %s", msg, err)})
						return &FailedInstallError{Message: msg, Cause: err}
					}
				}

				// Uninstall unused tools
				for _, toolRelease := range installedTools {
					if !pm.IsToolRequired(toolRelease) {
						log := pm.Log.WithField("Tool", toolRelease)

						log.Info("Uninstalling tool")
						taskCB(&rpc.TaskProgress{Name: tr("Uninstalling %s: tool is no more required", toolRelease)})

						if err := pm.UninstallTool(toolRelease); err != nil {
							log.WithError(err).Error("Error uninstalling")
							return &FailedInstallError{Message: tr("Error uninstalling tool %s", toolRelease), Cause: err}
						}

						log.Info("Tool uninstalled")
						taskCB(&rpc.TaskProgress{Message: tr("%s uninstalled", toolRelease), Completed: true})
					}
				}

				// Perform post install
				if !req.SkipPostInstall {
					logrus.Info("Running post_install script")
					taskCB(&rpc.TaskProgress{Message: tr("Configuring platform")})
					if err := pm.RunPostInstallScript(latest); err != nil {
						taskCB(&rpc.TaskProgress{Message: tr("WARNING: cannot run post install: %s", err)})
					}
				} else {
					logrus.Info("Skipping platform configuration (post_install run).")
					taskCB(&rpc.TaskProgress{Message: tr("Skipping platform configuration")})
				}
			}
		}
	}

	return nil
}

// LoadSketch collects and returns all files composing a sketch
func LoadSketch(ctx context.Context, req *rpc.LoadSketchRequest) (*rpc.LoadSketchResponse, error) {
	// TODO: This should be a ToRpc function for the Sketch struct
	sketch, err := sk.New(paths.New(req.SketchPath))
	if err != nil {
		return nil, &CantOpenSketchError{Cause: err}
	}

	otherSketchFiles := make([]string, sketch.OtherSketchFiles.Len())
	for i, file := range sketch.OtherSketchFiles {
		otherSketchFiles[i] = file.String()
	}

	additionalFiles := make([]string, sketch.AdditionalFiles.Len())
	for i, file := range sketch.AdditionalFiles {
		additionalFiles[i] = file.String()
	}

	rootFolderFiles := make([]string, sketch.RootFolderFiles.Len())
	for i, file := range sketch.RootFolderFiles {
		rootFolderFiles[i] = file.String()
	}

	return &rpc.LoadSketchResponse{
		MainFile:         sketch.MainFile.String(),
		LocationPath:     sketch.FullPath.String(),
		OtherSketchFiles: otherSketchFiles,
		AdditionalFiles:  additionalFiles,
		RootFolderFiles:  rootFolderFiles,
	}, nil
}
