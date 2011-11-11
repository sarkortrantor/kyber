#include "StartStop.hpp"

namespace Dissent {
namespace Utils {
  StartStop::StartStop() :
    _started(false),
    _stopped(false)
  {
  }

  bool StartStop::Start()
  {
    if(_started || _stopped) {
      return false;
    }

    _started = true;
    return true;
  }

  bool StartStop::Stop()
  {
    if(_stopped) {
      return false;
    }

    _stopped = true;
    return true;
  }

  void StartStop::DestructorCheck()
  {
    if(!Stopped() && Started()) {
      Stop();
    }
  }
}
}
